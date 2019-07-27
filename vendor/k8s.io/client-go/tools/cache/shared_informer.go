/*
Copyright 2015 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cache

import (
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/clock"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/buffer"

	"k8s.io/klog"
)

// SharedInformer has a shared data cache and is capable of distributing notifications for changes
// to the cache to multiple listeners who registered via AddEventHandler. If you use this, there is
// one behavior change compared to a standard Informer.  When you receive a notification, the cache
// will be AT LEAST as fresh as the notification, but it MAY be more fresh.  You should NOT depend
// on the contents of the cache exactly matching the notification you've received in handler
// functions.  If there was a create, followed by a delete, the cache may NOT have your item.  This
// has advantages over the broadcaster since it allows us to share a common cache across many
// controllers. Extending the broadcaster would have required us keep duplicate caches for each
// watch.
// SharedInformer有一个共享的数据缓存并且能够将缓存的变更分发到各个通过AddEventHandler注册到listeners
// 如果使用这个对象，它的一个行为和标准的Informer相比有所不同，当你收到一个通知，缓存至少喝这个通知一样新，可能更新
type SharedInformer interface {
	// AddEventHandler adds an event handler to the shared informer using the shared informer's resync
	// period.  Events to a single handler are delivered sequentially, but there is no coordination
	// between different handlers.
	AddEventHandler(handler ResourceEventHandler)
	// AddEventHandlerWithResyncPeriod adds an event handler to the shared informer using the
	// specified resync period.  Events to a single handler are delivered sequentially, but there is
	// no coordination between different handlers.
	AddEventHandlerWithResyncPeriod(handler ResourceEventHandler, resyncPeriod time.Duration)
	// GetStore returns the Store.
	GetStore() Store
	// GetController gives back a synthetic interface that "votes" to start the informer
	GetController() Controller
	// Run starts the shared informer, which will be stopped when stopCh is closed.
	// Run启动shared informer
	Run(stopCh <-chan struct{})
	// HasSynced returns true if the shared informer's store has synced.
	// HasSynced返回true，如果shared informer的store已经同步了
	HasSynced() bool
	// LastSyncResourceVersion is the resource version observed when last synced with the underlying
	// store. The value returned is not synchronized with access to the underlying store and is not
	// thread-safe.
	LastSyncResourceVersion() string
}

type SharedIndexInformer interface {
	SharedInformer
	// AddIndexers add indexers to the informer before it starts.
	// AddIndexers在informer启动之前往里面添加indexers
	AddIndexers(indexers Indexers) error
	// 获取相应的Indexer
	GetIndexer() Indexer
}

// NewSharedInformer creates a new instance for the listwatcher.
// NewSharedInformer为listwatcher创建一个新的实例
func NewSharedInformer(lw ListerWatcher, objType runtime.Object, resyncPeriod time.Duration) SharedInformer {
	return NewSharedIndexInformer(lw, objType, resyncPeriod, Indexers{})
}

// NewSharedIndexInformer creates a new instance for the listwatcher.
// NewSharedIndexInformer为listwatcher创建一个新的实例
func NewSharedIndexInformer(lw ListerWatcher, objType runtime.Object, defaultEventHandlerResyncPeriod time.Duration, indexers Indexers) SharedIndexInformer {
	realClock := &clock.RealClock{}
	sharedIndexInformer := &sharedIndexInformer{
		processor:                       &sharedProcessor{clock: realClock},
										 // DeletionHandlingMetaNamespaceKeyFunc是一个keyFunc，能够传入一个对象，得到它的key，一般为namespace/name的形式
		indexer:                         NewIndexer(DeletionHandlingMetaNamespaceKeyFunc, indexers),
		// listerWatcher就是资源对象和API Server进行List & Watch的函数
		listerWatcher:                   lw,
		objectType:                      objType,
		// resyncCheckPeriod和defaultEventHandlerResyncPeriod都是5min
		resyncCheckPeriod:               defaultEventHandlerResyncPeriod,
		defaultEventHandlerResyncPeriod: defaultEventHandlerResyncPeriod,
		cacheMutationDetector:           NewCacheMutationDetector(fmt.Sprintf("%T", objType)),
		clock:                           realClock,
	}
	return sharedIndexInformer
}

// InformerSynced is a function that can be used to determine if an informer has synced.  This is useful for determining if caches have synced.
type InformerSynced func() bool

const (
	// syncedPollPeriod controls how often you look at the status of your sync funcs
	syncedPollPeriod = 100 * time.Millisecond

	// initialBufferSize is the initial number of event notifications that can be buffered.
	// initialBufferSize是可以缓存的初始的event notifications的数目
	initialBufferSize = 1024
)

// WaitForCacheSync waits for caches to populate.  It returns true if it was successful, false
// if the controller should shutdown
func WaitForCacheSync(stopCh <-chan struct{}, cacheSyncs ...InformerSynced) bool {
	err := wait.PollUntil(syncedPollPeriod,
		func() (bool, error) {
			for _, syncFunc := range cacheSyncs {
				if !syncFunc() {
					return false, nil
				}
			}
			return true, nil
		},
		stopCh)
	if err != nil {
		klog.V(2).Infof("stop requested")
		return false
	}

	klog.V(4).Infof("caches populated")
	return true
}

type sharedIndexInformer struct {
	// indexer是真正缓存对象的地方
	indexer    Indexer
	controller Controller

	processor             *sharedProcessor
	cacheMutationDetector CacheMutationDetector

	// This block is tracked to handle late initialization of the controller
	listerWatcher ListerWatcher
	objectType    runtime.Object

	// resyncCheckPeriod is how often we want the reflector's resync timer to fire so it can call
	// shouldResync to check if any of our listeners need a resync.
	// resyncCheckPeriod是我们想要reflector的resync timer触发的频率，因此它可以调用shouldResync
	// 来检查是否我们的任何listeners需要一个resync
	resyncCheckPeriod time.Duration
	// defaultEventHandlerResyncPeriod is the default resync period for any handlers added via
	// AddEventHandler (i.e. they don't specify one and just want to use the shared informer's default
	// value).
	// defaultEventHandlerResyncPeriod是任何通过AddEventHandler添加的handlers的默认resync period
	defaultEventHandlerResyncPeriod time.Duration
	// clock allows for testability
	clock clock.Clock

	started, stopped bool
	startedLock      sync.Mutex

	// blockDeltas gives a way to stop all event distribution so that a late event handler
	// can safely join the shared informer.
	// blockDeltas能够停止所有的event notification，因此之后的一个event handler能够安全地加入shared informer
	blockDeltas sync.Mutex
}

// dummyController hides the fact that a SharedInformer is different from a dedicated one
// where a caller can `Run`.  The run method is disconnected in this case, because higher
// level logic will decide when to start the SharedInformer and related controller.
// Because returning information back is always asynchronous, the legacy callers shouldn't
// notice any change in behavior.
type dummyController struct {
	informer *sharedIndexInformer
}

func (v *dummyController) Run(stopCh <-chan struct{}) {
}

func (v *dummyController) HasSynced() bool {
	return v.informer.HasSynced()
}

func (c *dummyController) LastSyncResourceVersion() string {
	return ""
}

type updateNotification struct {
	oldObj interface{}
	newObj interface{}
}

type addNotification struct {
	newObj interface{}
}

type deleteNotification struct {
	oldObj interface{}
}

func (s *sharedIndexInformer) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()

	fifo := NewDeltaFIFO(MetaNamespaceKeyFunc, s.indexer)

	cfg := &Config{
		Queue:            fifo,
		ListerWatcher:    s.listerWatcher,
		ObjectType:       s.objectType,
		FullResyncPeriod: s.resyncCheckPeriod,
		RetryOnError:     false,
		ShouldResync:     s.processor.shouldResync,

		// Process方法上informer的HandleDeltas
		Process: s.HandleDeltas,
	}

	func() {
		s.startedLock.Lock()
		defer s.startedLock.Unlock()

		s.controller = New(cfg)
		s.controller.(*controller).clock = s.clock
		// 将s.started设置为true，表示已经SharedIndexInformer已经启动了
		s.started = true
	}()

	// Separate stop channel because Processor should be stopped strictly after controller
	// 分开stop channel，因为Processor应该严格地在controller之后停止
	processorStopCh := make(chan struct{})
	var wg wait.Group
	defer wg.Wait()              // Wait for Processor to stop
	defer close(processorStopCh) // Tell Processor to stop
	wg.StartWithChannel(processorStopCh, s.cacheMutationDetector.Run)
	wg.StartWithChannel(processorStopCh, s.processor.run)

	defer func() {
		s.startedLock.Lock()
		defer s.startedLock.Unlock()
		// 退出之后，将s.stopped设置为true，表示不想要任何新的listeners
		s.stopped = true // Don't want any new listeners
	}()
	s.controller.Run(stopCh)
}

func (s *sharedIndexInformer) HasSynced() bool {
	s.startedLock.Lock()
	defer s.startedLock.Unlock()

	if s.controller == nil {
		return false
	}
	return s.controller.HasSynced()
}

func (s *sharedIndexInformer) LastSyncResourceVersion() string {
	s.startedLock.Lock()
	defer s.startedLock.Unlock()

	if s.controller == nil {
		return ""
	}
	return s.controller.LastSyncResourceVersion()
}

// GetStore返回的就是sharedIndexInformer的indexer
func (s *sharedIndexInformer) GetStore() Store {
	return s.indexer
}

func (s *sharedIndexInformer) GetIndexer() Indexer {
	return s.indexer
}

func (s *sharedIndexInformer) AddIndexers(indexers Indexers) error {
	s.startedLock.Lock()
	defer s.startedLock.Unlock()

	if s.started {
		return fmt.Errorf("informer has already started")
	}

	return s.indexer.AddIndexers(indexers)
}

func (s *sharedIndexInformer) GetController() Controller {
	return &dummyController{informer: s}
}

func (s *sharedIndexInformer) AddEventHandler(handler ResourceEventHandler) {
	s.AddEventHandlerWithResyncPeriod(handler, s.defaultEventHandlerResyncPeriod)
}

func determineResyncPeriod(desired, check time.Duration) time.Duration {
	if desired == 0 {
		return desired
	}
	if check == 0 {
		klog.Warningf("The specified resyncPeriod %v is invalid because this shared informer doesn't support resyncing", desired)
		return 0
	}
	if desired < check {
		klog.Warningf("The specified resyncPeriod %v is being increased to the minimum resyncCheckPeriod %v", desired, check)
		return check
	}
	return desired
}

const minimumResyncPeriod = 1 * time.Second

func (s *sharedIndexInformer) AddEventHandlerWithResyncPeriod(handler ResourceEventHandler, resyncPeriod time.Duration) {
	s.startedLock.Lock()
	defer s.startedLock.Unlock()

	if s.stopped {
		// 如果shared informer停止了，则Handler不再加入
		klog.V(2).Infof("Handler %v was not added to shared informer because it has stopped already", handler)
		return
	}

	if resyncPeriod > 0 {
		// minimumResyncPeriod为1s
		if resyncPeriod < minimumResyncPeriod {
			// 如果minimumResyncPeriod太小，则调整为minimumResyncPeriod
			klog.Warningf("resyncPeriod %d is too small. Changing it to the minimum allowed value of %d", resyncPeriod, minimumResyncPeriod)
			resyncPeriod = minimumResyncPeriod
		}

		if resyncPeriod < s.resyncCheckPeriod {
			if s.started {
				klog.Warningf("resyncPeriod %d is smaller than resyncCheckPeriod %d and the informer has already started. Changing it to %d", resyncPeriod, s.resyncCheckPeriod, s.resyncCheckPeriod)
				resyncPeriod = s.resyncCheckPeriod
			} else {
				// if the event handler's resyncPeriod is smaller than the current resyncCheckPeriod, update
				// resyncCheckPeriod to match resyncPeriod and adjust the resync periods of all the listeners
				// accordingly
				// 如果event handler的resyncPeriod小于当前的resyncCheckPeriod，更新resyncCheckPeriod来匹配resyncPeriod
				// 并且相应地调整所有listeners的resync periods
				s.resyncCheckPeriod = resyncPeriod
				s.processor.resyncCheckPeriodChanged(resyncPeriod)
			}
		}
	}

	// 创建新的Process Listener
	listener := newProcessListener(handler, resyncPeriod, determineResyncPeriod(resyncPeriod, s.resyncCheckPeriod), s.clock.Now(), initialBufferSize)

	if !s.started {
		// 如果informer还没有启动，直接在processor中加入listener并返回
		s.processor.addListener(listener)
		return
	}

	// in order to safely join, we have to
	// 1. stop sending add/update/delete notifications
	// 2. do a list against the store
	// 3. send synthetic "Add" events to the new handler
	// 4. unblock
	// 为了能够将listener安全地加入，我们需要
	// 1. 停止发送add/update/delete通知
	// 2. 对store进行一次list
	// 3. 向新的handler发送一系列的"Add" events
	s.blockDeltas.Lock()
	defer s.blockDeltas.Unlock()

	s.processor.addListener(listener)
	for _, item := range s.indexer.List() {
		// 在listener中加入notification
		listener.add(addNotification{newObj: item})
	}
}

func (s *sharedIndexInformer) HandleDeltas(obj interface{}) error {
	s.blockDeltas.Lock()
	defer s.blockDeltas.Unlock()

	// from oldest to newest
	// 从最老到最新
	for _, d := range obj.(Deltas) {
		switch d.Type {
		case Sync, Added, Updated:
			// 如果Delta的类型为Sync，则isSync为true
			isSync := d.Type == Sync
			s.cacheMutationDetector.AddObject(d.Object)
			// 如果对象在indexer中存在，则更新indexer，并且分发到各个processor
			if old, exists, err := s.indexer.Get(d.Object); err == nil && exists {
				// 首先会对indexer进行更新
				if err := s.indexer.Update(d.Object); err != nil {
					return err
				}
				// 发送一个updateNotification
				// 不管isSync是否为true，都进行发送
				s.processor.distribute(updateNotification{oldObj: old, newObj: d.Object}, isSync)
			} else {
				// 否则在indexer中增加对象
				if err := s.indexer.Add(d.Object); err != nil {
					return err
				}
				// 发送一个addNotification
				s.processor.distribute(addNotification{newObj: d.Object}, isSync)
			}
		case Deleted:
			if err := s.indexer.Delete(d.Object); err != nil {
				return err
			}
			// 如果变更的类型为Deleted，则发送一个deleteNotification
			s.processor.distribute(deleteNotification{oldObj: d.Object}, false)
		}
	}
	return nil
}

// sharedProcessor中包含多个processorListner
type sharedProcessor struct {
	listenersStarted bool
	listenersLock    sync.RWMutex
	listeners        []*processorListener
	syncingListeners []*processorListener
	clock            clock.Clock
	wg               wait.Group
}

func (p *sharedProcessor) addListener(listener *processorListener) {
	p.listenersLock.Lock()
	defer p.listenersLock.Unlock()

	p.addListenerLocked(listener)
	if p.listenersStarted {
		// 将listener的run和pop函数加入wait.Group
		p.wg.Start(listener.run)
		p.wg.Start(listener.pop)
	}
}

func (p *sharedProcessor) addListenerLocked(listener *processorListener) {
	// 同时在listeners和syncingListeners中加入listener
	p.listeners = append(p.listeners, listener)
	p.syncingListeners = append(p.syncingListeners, listener)
}

func (p *sharedProcessor) distribute(obj interface{}, sync bool) {
	p.listenersLock.RLock()
	defer p.listenersLock.RUnlock()

	if sync {
		// 如果sync为true，加入syncingListeners
		for _, listener := range p.syncingListeners {
			listener.add(obj)
		}
	} else {
		// 如果sync为false，加入listeners
		for _, listener := range p.listeners {
			listener.add(obj)
		}
	}
}

// sharedProcessor的run函数启动各个listener的run和pop函数
// 这两个函数会等待add函数传来notification并处理
func (p *sharedProcessor) run(stopCh <-chan struct{}) {
	func() {
		p.listenersLock.RLock()
		defer p.listenersLock.RUnlock()
		// 遍历sharedProcessor的各个listeners
		for _, listener := range p.listeners {
			// 启动listener的各个run和pop函数
			p.wg.Start(listener.run)
			p.wg.Start(listener.pop)
		}
		p.listenersStarted = true
	}()
	<-stopCh
	p.listenersLock.RLock()
	defer p.listenersLock.RUnlock()
	for _, listener := range p.listeners {
		// 告诉.pop()函数停止，.pop()函数会让.run()停止
		close(listener.addCh) // Tell .pop() to stop. .pop() will tell .run() to stop
	}
	// 等待所有的.pop()和.run()函数停止
	p.wg.Wait() // Wait for all .pop() and .run() to stop
}

// shouldResync queries every listener to determine if any of them need a resync, based on each
// listener's resyncPeriod.
// shouldResync询问每个listener来决定它们是否需要resync，基于每个listener的resyncPeriod
func (p *sharedProcessor) shouldResync() bool {
	p.listenersLock.Lock()
	defer p.listenersLock.Unlock()

	p.syncingListeners = []*processorListener{}

	resyncNeeded := false
	now := p.clock.Now()
	for _, listener := range p.listeners {
		// need to loop through all the listeners to see if they need to resync so we can prepare any
		// listeners that are going to be resyncing.
		if listener.shouldResync(now) {
			resyncNeeded = true
			p.syncingListeners = append(p.syncingListeners, listener)
			listener.determineNextResync(now)
		}
	}
	return resyncNeeded
}

func (p *sharedProcessor) resyncCheckPeriodChanged(resyncCheckPeriod time.Duration) {
	p.listenersLock.RLock()
	defer p.listenersLock.RUnlock()

	for _, listener := range p.listeners {
		resyncPeriod := determineResyncPeriod(listener.requestedResyncPeriod, resyncCheckPeriod)
		listener.setResyncPeriod(resyncPeriod)
	}
}

type processorListener struct {
	nextCh chan interface{}
	addCh  chan interface{}

	handler ResourceEventHandler

	// pendingNotifications is an unbounded ring buffer that holds all notifications not yet distributed.
	// There is one per listener, but a failing/stalled listener will have infinite pendingNotifications
	// added until we OOM.
	// pendingNotifications时一个没有限制的ring buffer，其中包含了所有未被分发的notifications
	// 每个listener都有一个，但是一个failing/stalled listener会有着无限的pendingNotifications被添加直到OOM
	// TODO: This is no worse than before, since reflectors were backed by unbounded DeltaFIFOs, but
	// we should try to do something better.
	pendingNotifications buffer.RingGrowing

	// requestedResyncPeriod is how frequently the listener wants a full resync from the shared informer
	// requestedResyncPeriod时listener从shared informer进行full resync的频率
	requestedResyncPeriod time.Duration
	// resyncPeriod is how frequently the listener wants a full resync from the shared informer. This
	// value may differ from requestedResyncPeriod if the shared informer adjusts it to align with the
	// informer's overall resync check period.
	// resyncPeriod是listener从shared informer中进行一次full resync from的频率，它可能和requestResyncPeriod不同
	// 如果shared informer将它调整为何informer的整个resync check period一致
	resyncPeriod time.Duration
	// nextResync is the earliest time the listener should get a full resync
	nextResync time.Time
	// resyncLock guards access to resyncPeriod and nextResync
	resyncLock sync.Mutex
}

func newProcessListener(handler ResourceEventHandler, requestedResyncPeriod, resyncPeriod time.Duration, now time.Time, bufferSize int) *processorListener {
	ret := &processorListener{
		nextCh:                make(chan interface{}),
		addCh:                 make(chan interface{}),
		handler:               handler,
		pendingNotifications:  *buffer.NewRingGrowing(bufferSize),
		requestedResyncPeriod: requestedResyncPeriod,
		resyncPeriod:          resyncPeriod,
	}

	ret.determineNextResync(now)

	return ret
}

// add()方法往listener中添加notification
func (p *processorListener) add(notification interface{}) {
	p.addCh <- notification
}

// processorListener的pop()和run()之间其实也存在一个缓存，即pendingNotifications
// pop()会直接将notification放入pendingNotifications中，而run会从nextCh中读取notification
func (p *processorListener) pop() {
	defer utilruntime.HandleCrash()
	// 让.run()停止运行
	defer close(p.nextCh) // Tell .run() to stop

	var nextCh chan<- interface{}
	var notification interface{}
	for {
		select {
		case nextCh <- notification:
			// Notification dispatched
			// 分发notification
			var ok bool
			notification, ok = p.pendingNotifications.ReadOne()
			if !ok { // Nothing to pop
				// 如果pendingNotifications中没有notification需要pop
				// 则直接关闭当前的select case
				nextCh = nil // Disable this select case
			}
		case notificationToAdd, ok := <-p.addCh:
			// 从p.addCh中读取需要添加的notification
			if !ok {
				return
			}
			if notification == nil { // No notification to pop (and pendingNotifications is empty)
				// 如果当前没有Notificatio需要Pop（并且pendingNotifications为空）
				// Optimize the case - skip adding to pendingNotifications
				notification = notificationToAdd
				// 将nextCh设置为p.nextCh
				nextCh = p.nextCh
			} else { // There is already a notification waiting to be dispatched
				p.pendingNotifications.WriteOne(notificationToAdd)
			}
		}
	}
}

func (p *processorListener) run() {
	// this call blocks until the channel is closed.  When a panic happens during the notification
	// we will catch it, **the offending item will be skipped!**, and after a short delay (one second)
	// the next notification will be attempted.  This is usually better than the alternative of never
	// delivering again.
	stopCh := make(chan struct{})
	wait.Until(func() {
		// this gives us a few quick retries before a long pause and then a few more quick retries
		// 这让我们能在一个long pause之前进行一些重试并且之后能够进行更快地重拾
		err := wait.ExponentialBackoff(retry.DefaultRetry, func() (bool, error) {
			// 从p.nextCh中获取notification
			for next := range p.nextCh {
				switch notification := next.(type) {
					// 调用相应地handler进行处理
				case updateNotification:
					p.handler.OnUpdate(notification.oldObj, notification.newObj)
				case addNotification:
					p.handler.OnAdd(notification.newObj)
				case deleteNotification:
					p.handler.OnDelete(notification.oldObj)
				default:
					utilruntime.HandleError(fmt.Errorf("unrecognized notification: %#v", next))
				}
			}
			// the only way to get here is if the p.nextCh is empty and closed
			// 到达这里的唯一方法是
			return true, nil
		})

		// the only way to get here is if the p.nextCh is empty and closed
		if err == nil {
			close(stopCh)
		}
	}, 1*time.Minute, stopCh)
}

// shouldResync deterimines if the listener needs a resync. If the listener's resyncPeriod is 0,
// this always returns false.
// shouldResync决定是否listener需要一个resync，如果listener的resyncPeriod为0，则总是返回false
func (p *processorListener) shouldResync(now time.Time) bool {
	p.resyncLock.Lock()
	defer p.resyncLock.Unlock()

	if p.resyncPeriod == 0 {
		return false
	}

	return now.After(p.nextResync) || now.Equal(p.nextResync)
}

func (p *processorListener) determineNextResync(now time.Time) {
	p.resyncLock.Lock()
	defer p.resyncLock.Unlock()

	// 设置nextResync为当前时间加上resyncPeriod
	p.nextResync = now.Add(p.resyncPeriod)
}

func (p *processorListener) setResyncPeriod(resyncPeriod time.Duration) {
	p.resyncLock.Lock()
	defer p.resyncLock.Unlock()

	p.resyncPeriod = resyncPeriod
}

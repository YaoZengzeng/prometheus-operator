/*
Copyright 2014 The Kubernetes Authors.

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

	"k8s.io/apimachinery/pkg/util/sets"
)

// ThreadSafeStore is an interface that allows concurrent access to a storage backend.
// TL;DR caveats: you must not modify anything returned by Get or List as it will break
// the indexing feature in addition to not being thread safe.
// ThreadSafeStore是一个接口，允许并行地访问一个storage backend
// 保证不能修改Get或者List返回的anything，因为这会破坏indexing feature以及不再变得线程安全
//
// The guarantees of thread safety provided by List/Get are only valid if the caller
// treats returned items as read-only. For example, a pointer inserted in the store
// through `Add` will be returned as is by `Get`. Multiple clients might invoke `Get`
// on the same key and modify the pointer in a non-thread-safe way. Also note that
// modifying objects stored by the indexers (if any) will *not* automatically lead
// to a re-index. So it's not a good idea to directly modify the objects returned by
// Get/List, in general.
// 修改由indexers存储的objects不会自动导致re-index，因此一般来说直接修改由Get/List返回的对象并不是
// 一个好的方法
type ThreadSafeStore interface {
	Add(key string, obj interface{})
	Update(key string, obj interface{})
	Delete(key string)
	Get(key string) (item interface{}, exists bool)
	List() []interface{}
	ListKeys() []string
	Replace(map[string]interface{}, string)
	Index(indexName string, obj interface{}) ([]interface{}, error)
	IndexKeys(indexName, indexKey string) ([]string, error)
	ListIndexFuncValues(name string) []string
	ByIndex(indexName, indexKey string) ([]interface{}, error)
	GetIndexers() Indexers

	// AddIndexers adds more indexers to this store.  If you call this after you already have data
	// in the store, the results are undefined.
	// AddIndexeers在store中增加更多的indexers，如果在store中已经有数据的情况下调用这个方法
	// 结果将是不确定的
	AddIndexers(newIndexers Indexers) error
	Resync() error
}

// threadSafeMap implements ThreadSafeStore
type threadSafeMap struct {
	lock  sync.RWMutex
	items map[string]interface{}

	// indexers maps a name to an IndexFunc
	indexers Indexers
	// indices maps a name to an Index
	// indices其实就是多个Index，将name映射到一个Index
	indices Indices
}

func (c *threadSafeMap) Add(key string, obj interface{}) {
	c.lock.Lock()
	defer c.lock.Unlock()
	oldObject := c.items[key]
	c.items[key] = obj
	c.updateIndices(oldObject, obj, key)
}

func (c *threadSafeMap) Update(key string, obj interface{}) {
	c.lock.Lock()
	defer c.lock.Unlock()
	oldObject := c.items[key]
	c.items[key] = obj
	c.updateIndices(oldObject, obj, key)
}

func (c *threadSafeMap) Delete(key string) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if obj, exists := c.items[key]; exists {
		c.deleteFromIndices(obj, key)
		delete(c.items, key)
	}
}

func (c *threadSafeMap) Get(key string) (item interface{}, exists bool) {
	c.lock.RLock()
	defer c.lock.RUnlock()
	item, exists = c.items[key]
	return item, exists
}

func (c *threadSafeMap) List() []interface{} {
	c.lock.RLock()
	defer c.lock.RUnlock()
	list := make([]interface{}, 0, len(c.items))
	for _, item := range c.items {
		list = append(list, item)
	}
	return list
}

// ListKeys returns a list of all the keys of the objects currently
// in the threadSafeMap.
func (c *threadSafeMap) ListKeys() []string {
	c.lock.RLock()
	defer c.lock.RUnlock()
	list := make([]string, 0, len(c.items))
	for key := range c.items {
		list = append(list, key)
	}
	return list
}

func (c *threadSafeMap) Replace(items map[string]interface{}, resourceVersion string) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.items = items

	// rebuild any index
	c.indices = Indices{}
	for key, item := range c.items {
		c.updateIndices(nil, item, key)
	}
}

// Index returns a list of items that match on the index function
// Index is thread-safe so long as you treat all items as immutable
func (c *threadSafeMap) Index(indexName string, obj interface{}) ([]interface{}, error) {
	c.lock.RLock()
	defer c.lock.RUnlock()

	// 根据indexName找到相应的indexFunc，再利用这个indexFunc计算出对应的indexKeys
	indexFunc := c.indexers[indexName]
	if indexFunc == nil {
		return nil, fmt.Errorf("Index with name %s does not exist", indexName)
	}

	indexKeys, err := indexFunc(obj)
	if err != nil {
		return nil, err
	}
	index := c.indices[indexName]

	// 例如当indexName为"namespace"时，返回obj所在的namespace下的所有同类型对象
	var returnKeySet sets.String
	if len(indexKeys) == 1 {
		// In majority of cases, there is exactly one value matching.
		// Optimize the most common path - deduping is not needed here.
		returnKeySet = index[indexKeys[0]]
	} else {
		// Need to de-dupe the return list.
		// Since multiple keys are allowed, this can happen.
		returnKeySet = sets.String{}
		for _, indexKey := range indexKeys {
			for key := range index[indexKey] {
				returnKeySet.Insert(key)
			}
		}
	}

	list := make([]interface{}, 0, returnKeySet.Len())
	// 返回所有的items
	for absoluteKey := range returnKeySet {
		list = append(list, c.items[absoluteKey])
	}
	return list, nil
}

// ByIndex returns a list of items that match an exact value on the index function
func (c *threadSafeMap) ByIndex(indexName, indexKey string) ([]interface{}, error) {
	c.lock.RLock()
	defer c.lock.RUnlock()

	indexFunc := c.indexers[indexName]
	if indexFunc == nil {
		return nil, fmt.Errorf("Index with name %s does not exist", indexName)
	}

	index := c.indices[indexName]

	set := index[indexKey]
	list := make([]interface{}, 0, set.Len())
	for _, key := range set.List() {
		list = append(list, c.items[key])
	}

	return list, nil
}

// IndexKeys returns a list of keys that match on the index function.
// IndexKeys is thread-safe so long as you treat all items as immutable.
func (c *threadSafeMap) IndexKeys(indexName, indexKey string) ([]string, error) {
	c.lock.RLock()
	defer c.lock.RUnlock()

	indexFunc := c.indexers[indexName]
	if indexFunc == nil {
		return nil, fmt.Errorf("Index with name %s does not exist", indexName)
	}

	index := c.indices[indexName]

	set := index[indexKey]
	return set.List(), nil
}

func (c *threadSafeMap) ListIndexFuncValues(indexName string) []string {
	c.lock.RLock()
	defer c.lock.RUnlock()

	index := c.indices[indexName]
	names := make([]string, 0, len(index))
	for key := range index {
		names = append(names, key)
	}
	return names
}

func (c *threadSafeMap) GetIndexers() Indexers {
	return c.indexers
}

func (c *threadSafeMap) AddIndexers(newIndexers Indexers) error {
	c.lock.Lock()
	defer c.lock.Unlock()

	if len(c.items) > 0 {
		return fmt.Errorf("cannot add indexers to running index")
	}

	oldKeys := sets.StringKeySet(c.indexers)
	newKeys := sets.StringKeySet(newIndexers)

	if oldKeys.HasAny(newKeys.List()...) {
		return fmt.Errorf("indexer conflict: %v", oldKeys.Intersection(newKeys))
	}

	for k, v := range newIndexers {
		c.indexers[k] = v
	}
	return nil
}

// updateIndices modifies the objects location in the managed indexes, if this is an update, you must provide an oldObj
// updateIndices must be called from a function that already has a lock on the cache
// updateIndices修改在managed indexes的对象的位置
func (c *threadSafeMap) updateIndices(oldObj interface{}, newObj interface{}, key string) {
	// if we got an old object, we need to remove it before we add it again
	if oldObj != nil {
		c.deleteFromIndices(oldObj, key)
	}
	// 一般也就一个indexer，name为"namespace"，indexFunc用来获取对象的namespace
	for name, indexFunc := range c.indexers {
		// 调用indexFunc找出对象的indexValues
		indexValues, err := indexFunc(newObj)
		if err != nil {
			panic(fmt.Errorf("unable to calculate an index entry for key %q on index %q: %v", key, name, err))
		}
		// 如果indices[name]为空，则创建一个
		// 此处即为indices["namespace"]
		index := c.indices[name]
		if index == nil {
			index = Index{}
			c.indices[name] = index
		}

		// 遍历各个indexValues，
		// 例如在default这个namespace有个对象abc
		// 它的key为default/abc
		// 那么它在索引中的位置为c.indices["namespace"]["default"]={"default/abc"}
		// 这样我们基于某个namespace进行查找的话，就能很快找到某个对象
		for _, indexValue := range indexValues {
			set := index[indexValue]
			// 如果对应的indexValue还不存在，就创建一个
			if set == nil {
				set = sets.String{}
				index[indexValue] = set
			}
			// 将对象的key插入
			set.Insert(key)
		}
	}
}

// deleteFromIndices removes the object from each of the managed indexes
// it is intended to be called from a function that already has a lock on the cache
func (c *threadSafeMap) deleteFromIndices(obj interface{}, key string) {
	for name, indexFunc := range c.indexers {
		indexValues, err := indexFunc(obj)
		if err != nil {
			panic(fmt.Errorf("unable to calculate an index entry for key %q on index %q: %v", key, name, err))
		}

		index := c.indices[name]
		if index == nil {
			continue
		}
		for _, indexValue := range indexValues {
			set := index[indexValue]
			if set != nil {
				set.Delete(key)
			}
		}
	}
}

func (c *threadSafeMap) Resync() error {
	// Nothing to do
	return nil
}

func NewThreadSafeStore(indexers Indexers, indices Indices) ThreadSafeStore {
	return &threadSafeMap{
		items:    map[string]interface{}{},
		indexers: indexers,
		indices:  indices,
	}
}

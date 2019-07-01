/*
Copyright 2016 The Kubernetes Authors.

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

package workqueue

// RateLimitingInterface is an interface that rate limits items being added to the queue.
// RateLimitingInterface是一个接口，限制item加入队列的速率
type RateLimitingInterface interface {
	// 基础的DelayingInterface
	DelayingInterface

	// AddRateLimited adds an item to the workqueue after the rate limiter says it's ok
	// AddRateLimited将一个item加入workqueue，在rate limiter说明OK以后
	AddRateLimited(item interface{})

	// Forget indicates that an item is finished being retried.  Doesn't matter whether it's for perm failing
	// or for success, we'll stop the rate limiter from tracking it.  This only clears the `rateLimiter`, you
	// still have to call `Done` on the queue.
	// Forget只是清除了rateLimiter，我们还是要在队列中调用`Done`
	Forget(item interface{})

	// NumRequeues returns back how many times the item was requeued
	// NemRequeues返回有多少items被重新入队
	NumRequeues(item interface{}) int
}

// NewRateLimitingQueue constructs a new workqueue with rateLimited queuing ability
// Remember to call Forget!  If you don't, you may end up tracking failures forever.
// NewRateLimitingQueue构造一个新的有着限速能力的workqueue
// 记住要调用Forget，如果没有的话，你可能一直追踪failures
func NewRateLimitingQueue(rateLimiter RateLimiter) RateLimitingInterface {
	return &rateLimitingType{
		DelayingInterface: NewDelayingQueue(),
		rateLimiter:       rateLimiter,
	}
}

func NewNamedRateLimitingQueue(rateLimiter RateLimiter, name string) RateLimitingInterface {
	return &rateLimitingType{
		DelayingInterface: NewNamedDelayingQueue(name),
		rateLimiter:       rateLimiter,
	}
}

// rateLimitingType wraps an Interface and provides rateLimited re-enquing
// rateLimitingType封装了一个Interface并且提供限速的re-enquing
type rateLimitingType struct {
	DelayingInterface

	rateLimiter RateLimiter
}

// AddRateLimited AddAfter's the item based on the time when the rate limiter says it's ok
// AddRateLimited基于rate limiter提供的时间，对item进行AddAfter
func (q *rateLimitingType) AddRateLimited(item interface{}) {
	q.DelayingInterface.AddAfter(item, q.rateLimiter.When(item))
}

func (q *rateLimitingType) NumRequeues(item interface{}) int {
	return q.rateLimiter.NumRequeues(item)
}

func (q *rateLimitingType) Forget(item interface{}) {
	q.rateLimiter.Forget(item)
}

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

// Package workqueue provides a simple queue that supports the following
// features:
// workqueue包提供了一个包含如下特性的简单的队列：
//  * Fair: items processed in the order in which they are added.
//	* 公平：items以它们加入队列的顺序被处理
//  * Stingy: a single item will not be processed multiple times concurrently,
//      and if an item is added multiple times before it can be processed, it
//      will only be processed once.
//	* Stingy: 单个的item不会被并发处理很多次，如果一个item在被处理之前添加到队列多次，它最后
//		之后被处理一次
//  * Multiple consumers and producers. In particular, it is allowed for an
//      item to be reenqueued while it is being processed.
//	* 多个消费者和生产者，特别地，允许一个item在它被处理的时候重新入队
//  * Shutdown notifications.
//	* 关闭通知
package workqueue

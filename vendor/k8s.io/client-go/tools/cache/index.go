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

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/util/sets"
)

// Indexer is a storage interface that lets you list objects using multiple indexing functions
// Indexer是一个存储接口，它允许你使用多个indexing function来list objects
type Indexer interface {
	Store
	// Retrieve list of objects that match on the named indexing function
	// 获取一系列的objects，匹配named indexing function
	Index(indexName string, obj interface{}) ([]interface{}, error)
	// IndexKeys returns the set of keys that match on the named indexing function.
	IndexKeys(indexName, indexKey string) ([]string, error)
	// ListIndexFuncValues returns the list of generated values of an Index func
	ListIndexFuncValues(indexName string) []string
	// ByIndex lists object that match on the named indexing function with the exact key
	ByIndex(indexName, indexKey string) ([]interface{}, error)
	// GetIndexer return the indexers
	GetIndexers() Indexers

	// AddIndexers adds more indexers to this store.  If you call this after you already have data
	// in the store, the results are undefined.
	// AddIndexers在store中增加更多的indexers，如果在store中已经有data之后再调用这个函数，结果是未定义的
	AddIndexers(newIndexers Indexers) error
}

// IndexFunc knows how to provide an indexed value for an object.
// IndexFunc知道如何为一个object提供indexed value
type IndexFunc func(obj interface{}) ([]string, error)

// IndexFuncToKeyFuncAdapter adapts an indexFunc to a keyFunc.  This is only useful if your index function returns
// unique values for every object.  This is conversion can create errors when more than one key is found.  You
// should prefer to make proper key and index functions.
func IndexFuncToKeyFuncAdapter(indexFunc IndexFunc) KeyFunc {
	return func(obj interface{}) (string, error) {
		indexKeys, err := indexFunc(obj)
		if err != nil {
			return "", err
		}
		if len(indexKeys) > 1 {
			return "", fmt.Errorf("too many keys: %v", indexKeys)
		}
		if len(indexKeys) == 0 {
			return "", fmt.Errorf("unexpected empty indexKeys")
		}
		return indexKeys[0], nil
	}
}

const (
	NamespaceIndex string = "namespace"
)

// MetaNamespaceIndexFunc is a default index function that indexes based on an object's namespace
// MetaNamespaceIndexFunc上一个默认的index function，它基于对象的namespace进行索引
// 这个函数就是返回obj的namespace
func MetaNamespaceIndexFunc(obj interface{}) ([]string, error) {
	meta, err := meta.Accessor(obj)
	if err != nil {
		return []string{""}, fmt.Errorf("object has no meta: %v", err)
	}
	return []string{meta.GetNamespace()}, nil
}

// Index maps the indexed value to a set of keys in the store that match on that value
// Index将value映射到一系列store中的匹配这些value的keys
type Index map[string]sets.String

// Indexers maps a name to a IndexFunc
// Indexers将一个name映射到一个IndexFunc
type Indexers map[string]IndexFunc

// Indices maps a name to an Index
// Indices将一个name映射到一个Index
type Indices map[string]Index

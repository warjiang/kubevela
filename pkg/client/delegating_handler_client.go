/*
Copyright 2021 The KubeVela Authors.

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

package client

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DelegatingHandlerClient override the original client's function
// DelegatingHandlerClient 实现了client.Client接口，但是它的Get和List方法是通过委托的方式实现的，如果构造代理对象的时候传入了get和list
// 方法则get、list是调用用户自己实现的方法否则调用client.CLient的默认实现
type DelegatingHandlerClient struct {
	client.Client
	Getter func(ctx context.Context, key client.ObjectKey, obj client.Object) error
	Lister func(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error
}

// Get resource by overridden getter
func (c DelegatingHandlerClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object) error {
	if c.Getter != nil {
		return c.Getter(ctx, key, obj)
	}
	return c.Client.Get(ctx, key, obj)
}

// List resource by overridden lister
func (c DelegatingHandlerClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if c.Lister != nil {
		return c.Lister(ctx, list, opts...)
	}
	return c.Client.List(ctx, list, opts...)
}

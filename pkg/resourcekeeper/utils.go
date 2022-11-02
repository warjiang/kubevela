/*
Copyright 2022 The KubeVela Authors.

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

package resourcekeeper

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/oam-dev/kubevela/pkg/utils"
)

// ClearNamespaceForClusterScopedResources clear namespace for cluster scoped resources
func (h *resourceKeeper) ClearNamespaceForClusterScopedResources(manifests []*unstructured.Unstructured) {
	// 集群级别资源需要移除 namespace, 即设置namespace为空
	// 遍历所有的资源，对于cluster级别的资源移除namespace
	for _, manifest := range manifests {
		if ok, err := utils.IsClusterScope(manifest.GroupVersionKind(), h.Client.RESTMapper()); err == nil && ok {
			manifest.SetNamespace("")
		}
	}
}

func (h *resourceKeeper) isShared(manifest *unstructured.Unstructured) bool {
	// sharedResourcePolicy 为空情况下跳过检查
	if h.sharedResourcePolicy == nil {
		return false
	}
	//
	return h.sharedResourcePolicy.FindStrategy(manifest)
}

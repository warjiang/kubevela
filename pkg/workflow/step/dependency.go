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

package step

import (
	"context"

	"github.com/pkg/errors"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	types2 "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1alpha1"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/utils"
)

// LoadExternalPoliciesForWorkflow detects policies used in workflow steps which are not declared in internal policies
// try to load them from external policy objects in the application's namespace
func LoadExternalPoliciesForWorkflow(ctx context.Context, cli client.Client, appNs string, steps []v1beta1.WorkflowStep, internalPolicies []v1beta1.AppPolicy) ([]v1beta1.AppPolicy, error) {
	// internalPolicies 作个副本到 policies, 遍历policies数组，完成数组转map操作
	policies := internalPolicies
	policyMap := map[string]struct{}{}
	for _, policy := range policies {
		// 传入的policies是application.Spec上定义的polocies
		policyMap[policy.Name] = struct{}{}
	}
	/*
	// Load extra used policies declared in the workflow step
	// 遍历workflow上所有的policy，只有deploy类型的workflowstep才可能存在policy
	workflow:
		steps:
		- type: deploy
	      name: deploy-local
	      properties:
			policies: ["topology-local"]
		- type: deploy
	      name: deploy-hangzhou
	      properties:
			# require manual approval before running this step
			auto: false
			policies: ["topology-hangzhou-clusters"]
	*/
	for _, _step := range steps {
		// 只关心type=deploy且Properties不为空的step
		if _step.Type == DeployWorkflowStep && _step.Properties != nil {
			props := DeployWorkflowStepSpec{}
			if err := utils.StrictUnmarshal(_step.Properties.Raw, &props); err != nil {
				return nil, errors.Wrapf(err, "invalid WorkflowStep %s", _step.Name)
			}
			// 遍历step的policies，如果不在policyMap中，则调用k8s api根据ns/policyName获取policy对象, 一份追加到policy数组中, 一份写入到policyMap中
			for _, policyName := range props.Policies {
				if _, found := policyMap[policyName]; !found {
					po := &v1alpha1.Policy{}
					if err := cli.Get(ctx, types2.NamespacedName{Namespace: appNs, Name: policyName}, po); err != nil {
						if kerrors.IsNotFound(err) {
							return nil, errors.Errorf("external policy %s not found", policyName)
						}
						return nil, errors.Wrapf(err, "failed to load external policy %s in namespace %s", policyName, appNs)
					}
					policies = append(policies, v1beta1.AppPolicy{Name: policyName, Type: po.Type, Properties: po.Properties})
					policyMap[policyName] = struct{}{}
				}
			}
		}
	}
	return policies, nil
}

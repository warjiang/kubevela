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
	"encoding/json"
	"reflect"

	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1alpha1"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/oam/util"
	"github.com/oam-dev/kubevela/pkg/utils"
	wftypes "github.com/oam-dev/kubevela/pkg/workflow/types"
)

// WorkflowStepGenerator generator generates workflow steps
type WorkflowStepGenerator interface {
	Generate(app *v1beta1.Application, existingSteps []v1beta1.WorkflowStep) ([]v1beta1.WorkflowStep, error)
}

// ChainWorkflowStepGenerator chains multiple workflow step generators
type ChainWorkflowStepGenerator struct {
	generators []WorkflowStepGenerator
}

// Generate generate workflow steps
func (g *ChainWorkflowStepGenerator) Generate(app *v1beta1.Application, existingSteps []v1beta1.WorkflowStep) (steps []v1beta1.WorkflowStep, err error) {
	// 输入existingSteps,一般为空数组
	// 遍历所有的step generator, 传入app和existingSteps，返回最新的steps, 迭代完所有的step generator即可得到最终的steps
	steps = existingSteps
	for _, generator := range g.generators {
		steps, err = generator.Generate(app, steps)
		if err != nil {
			return steps, errors.Wrapf(err, "generate step failed in WorkflowStepGenerator %s", reflect.TypeOf(generator).Name())
		}
	}
	return steps, nil
}

// NewChainWorkflowStepGenerator create ChainWorkflowStepGenerator
func NewChainWorkflowStepGenerator(generators ...WorkflowStepGenerator) WorkflowStepGenerator {
	// 先收集所有的 step generator
	return &ChainWorkflowStepGenerator{generators: generators}
}

// RefWorkflowStepGenerator generate workflow steps from ref workflow
type RefWorkflowStepGenerator struct {
	context.Context
	client.Client
}

// Generate generate workflow steps
func (g *RefWorkflowStepGenerator) Generate(app *v1beta1.Application, existingSteps []v1beta1.WorkflowStep) (steps []v1beta1.WorkflowStep, err error) {
	// TODO refwork flow怎么用还不清楚呢-_-
	// 这里 ref workflow 应该是创建 application 时候创建的关联的workflow, application.Spec.Workflow.Ref 仅仅是关联该workflow
	if app.Spec.Workflow == nil || app.Spec.Workflow.Ref == "" {
		// 新创建的application应该都是这个逻辑，直接返回
		return existingSteps, nil
	}
	if app.Spec.Workflow.Steps != nil {
		return nil, errors.Errorf("cannot set steps and ref in workflow at the same time")
	}
	wf := &v1alpha1.Workflow{}
	if err = g.Client.Get(g.Context, types.NamespacedName{Namespace: app.GetNamespace(), Name: app.Spec.Workflow.Ref}, wf); err != nil {
		return
	}
	// 传入当前wf.Steps遍历数组、强转类型，转换成v1beta1.WorkflowStep[]
	return ConvertSteps(wf.Steps), nil
}

// ApplyComponentWorkflowStepGenerator generate apply-component workflow steps for all components in the application
type ApplyComponentWorkflowStepGenerator struct{}

// Generate generate workflow steps
func (g *ApplyComponentWorkflowStepGenerator) Generate(app *v1beta1.Application, existingSteps []v1beta1.WorkflowStep) (steps []v1beta1.WorkflowStep, err error) {
	if len(existingSteps) > 0 {
		return existingSteps, nil
	}
	// 为所有的component生成 workflow step
	for _, comp := range app.Spec.Components {
		steps = append(steps, v1beta1.WorkflowStep{
			Name: comp.Name,
			Type: wftypes.WorkflowStepTypeApplyComponent,
			Properties: util.Object2RawExtension(map[string]string{
				"component": comp.Name,
			}),
		})
	}
	return
}

// Deploy2EnvWorkflowStepGenerator generate deploy2env workflow steps for all envs in the application
type Deploy2EnvWorkflowStepGenerator struct{}

// Generate generate workflow steps
func (g *Deploy2EnvWorkflowStepGenerator) Generate(app *v1beta1.Application, existingSteps []v1beta1.WorkflowStep) (steps []v1beta1.WorkflowStep, err error) {
	if len(existingSteps) > 0 {
		return existingSteps, nil
	}
	// 遍历所有的policy
	// 所有policy下的"env-binding"类型的项都会转换成deploy2env的workflow step
	for _, policy := range app.Spec.Policies {
		// 找 type="env-binding" 的policy 且 Properties 不为空的case
		if policy.Type == v1alpha1.EnvBindingPolicyType && policy.Properties != nil {
			spec := &v1alpha1.EnvBindingSpec{}
			// Properties 反序列化成 EnvBindingSpec 对象
			if err = json.Unmarshal(policy.Properties.Raw, spec); err != nil {
				return
			}
			// 遍历spec.Envs
			for _, env := range spec.Envs {
				// env -> step
				steps = append(steps, v1beta1.WorkflowStep{
					Name: "deploy-" + policy.Name + "-" + env.Name,
					Type: "deploy2env",
					Properties: util.Object2RawExtension(map[string]string{
						"policy": policy.Name,
						"env":    env.Name,
					}),
				})
			}
		}
	}
	return
}

// DeployWorkflowStepGenerator generate deploy workflow steps for all topology & override in the application
type DeployWorkflowStepGenerator struct{}

// Generate generate workflow steps
func (g *DeployWorkflowStepGenerator) Generate(app *v1beta1.Application, existingSteps []v1beta1.WorkflowStep) (steps []v1beta1.WorkflowStep, err error) {
	// TODO 1. policy 命中的条件
	if len(existingSteps) > 0 {
		return existingSteps, nil
	}
	var topologies []string
	var overrides []string
	// TODO 2.topology、override 的 policy 似乎是对全部 component 生效
	// 这里的实现似乎是会遍历所有的policy
	// 所有的topology收集到topologies数组中
	// 所有的override收集到overrides数组中
	for _, policy := range app.Spec.Policies {
		switch policy.Type {
		case v1alpha1.TopologyPolicyType:
			topologies = append(topologies, policy.Name)
		case v1alpha1.OverridePolicyType:
			overrides = append(overrides, policy.Name)
		}
	}
	// 遍历所有的topology,为每个topology创建一个deploy类型的step
	for _, topology := range topologies {
		// topology -> workflow step
		steps = append(steps, v1beta1.WorkflowStep{
			Name: "deploy-" + topology,
			Type: "deploy",
			Properties: util.Object2RawExtension(map[string]interface{}{
				"policies": append(overrides, topology),
			}),
		})
	}
	if len(topologies) == 0 {
		containsRefObjects := false
		// 遍历判断是否存在 ref-objects
		// 关键现在都没有type=ref-objects了
		// https://kubevela.net/zh/docs/end-user/components/references
		for _, comp := range app.Spec.Components {
			if comp.Type == v1alpha1.RefObjectsComponentType {
				containsRefObjects = true
				break
			}
		}
		// TODO containsRefObjects必为false
		if containsRefObjects || len(overrides) > 0 {
			steps = append(steps, v1beta1.WorkflowStep{
				Name:       "deploy",
				Type:       "deploy",
				Properties: util.Object2RawExtension(map[string]interface{}{
					"policies": append([]string{}, overrides...),
				}),
			})
		}
	}
	return steps, nil
}

// DeployPreApproveWorkflowStepGenerator generate suspend workflow steps before all deploy steps
type DeployPreApproveWorkflowStepGenerator struct{}

// Generate generate workflow steps
func (g *DeployPreApproveWorkflowStepGenerator) Generate(app *v1beta1.Application, existingSteps []v1beta1.WorkflowStep) (steps []v1beta1.WorkflowStep, err error) {
	lastSuspend := false
	for _, step := range existingSteps {
		// 只处理type=deploy的step
		if step.Type == "deploy" && !lastSuspend {
			props := DeployWorkflowStepSpec{}
			if step.Properties != nil {
				_ = utils.StrictUnmarshal(step.Properties.Raw, &props)
			}
			// 如果step没配置Auto字段，则表示默认放行。如果配置了Auto字段，且Auto字段为false的话，则插入一个type=suspend的step
			if props.Auto != nil && !*props.Auto {
				steps = append(steps, v1beta1.WorkflowStep{
					Name: "manual-approve-" + step.Name,
					Type: wftypes.WorkflowStepTypeSuspend,
				})
			}
		}
		// 更新lastSuspend，如果前面已经有一个type=suspend的step，则lastSuspend为true,后面暂时不需要插入type=suspend的step
		lastSuspend = step.Type == wftypes.WorkflowStepTypeSuspend
		// 无论是否需要suspend都得将当前step插入到steps列表中
		steps = append(steps, step)
	}
	return steps, nil
}

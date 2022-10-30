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

package template

import (
	"context"
	"embed"
	"fmt"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/apis/types"
	"github.com/oam-dev/kubevela/pkg/appfile"
	"github.com/oam-dev/kubevela/pkg/oam/discoverymapper"
)

var (
	//go:embed static
	templateFS embed.FS
)

const (
	templateDir = "static"
)

// Loader load task definition template.
type Loader interface {
	LoadTaskTemplate(ctx context.Context, name string) (string, error)
}

// WorkflowStepLoader load workflowStep task definition template.
type WorkflowStepLoader struct {
	loadCapabilityDefinition func(ctx context.Context, capName string) (*appfile.Template, error)
}

// LoadTaskTemplate gets the workflowStep definition.
func (loader *WorkflowStepLoader) LoadTaskTemplate(ctx context.Context, name string) (string, error) {
	// template 通过 go:embed 静态嵌入 pkg/workflow/tasks/template/static 目录
	files, err := templateFS.ReadDir(templateDir)
	if err != nil {
		return "", err
	}
	// 根据task name组装出cue文件名
	// 如果找到了cue文件，直接读取返回
	staticFilename := name + ".cue"
	for _, file := range files {
		if staticFilename == file.Name() {
			fileName := fmt.Sprintf("%s/%s", templateDir, file.Name())
			content, err := templateFS.ReadFile(fileName)
			return string(content), err
		}
	}

	// static目录下找不到对应的cue文件则调用appfile的LoadTemplateFromRevision方法(pkg/appfile/template.go:157)从crd中加载对应的task template
	templ, err := loader.loadCapabilityDefinition(ctx, name)
	if err != nil {
		return "", err
	}
	// 返回结果为cue模板
	schematic := templ.WorkflowStepDefinition.Spec.Schematic
	if schematic != nil && schematic.CUE != nil {
		return schematic.CUE.Template, nil
	}

	return "", errors.New("custom workflowStep only support cue")
}

// NewWorkflowStepTemplateLoader create a task template loader.
func NewWorkflowStepTemplateLoader(client client.Client, dm discoverymapper.DiscoveryMapper) Loader {
	return &WorkflowStepLoader{
		loadCapabilityDefinition: func(ctx context.Context, capName string) (*appfile.Template, error) {
			return appfile.LoadTemplate(ctx, dm, client, capName, types.TypeWorkflowStep)
		},
	}
}

// NewWorkflowStepTemplateRevisionLoader create a task template loader from ApplicationRevision.
func NewWorkflowStepTemplateRevisionLoader(rev *v1beta1.ApplicationRevision, dm discoverymapper.DiscoveryMapper) Loader {
	return &WorkflowStepLoader{
		loadCapabilityDefinition: func(ctx context.Context, capName string) (*appfile.Template, error) {
			// 写死为 "workflowstep"
			return appfile.LoadTemplateFromRevision(capName, types.TypeWorkflowStep, rev, dm)
		},
	}
}

// ViewLoader load view task definition template.
type ViewLoader struct {
	client    client.Client
	namespace string
}

// LoadTaskTemplate gets the workflowStep definition.
func (loader *ViewLoader) LoadTaskTemplate(ctx context.Context, name string) (string, error) {
	cm := new(corev1.ConfigMap)
	cmKey := client.ObjectKey{Name: name, Namespace: loader.namespace}
	if err := loader.client.Get(ctx, cmKey, cm); err != nil {
		return "", errors.Wrapf(err, "fail to get view template %v from configMap", cmKey)
	}
	return cm.Data["template"], nil
}

// NewViewTemplateLoader create a view task template loader.
func NewViewTemplateLoader(client client.Client, namespace string) Loader {
	return &ViewLoader{
		client:    client,
		namespace: namespace,
	}
}

// EchoLoader will load data from input as it is.
type EchoLoader struct {
}

// LoadTaskTemplate gets the echo content exactly what it is .
func (ll *EchoLoader) LoadTaskTemplate(_ context.Context, content string) (string, error) {
	return content, nil
}

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

package addon

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"cuelang.org/go/cue/parser"
	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	common2 "github.com/oam-dev/kubevela/apis/core.oam.dev/common"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1alpha1"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/apis/types"
	cuemodel "github.com/oam-dev/kubevela/pkg/cue/model"
	"github.com/oam-dev/kubevela/pkg/cue/model/value"
	"github.com/oam-dev/kubevela/pkg/multicluster"
	"github.com/oam-dev/kubevela/pkg/oam"
	"github.com/oam-dev/kubevela/pkg/oam/util"
	addonutil "github.com/oam-dev/kubevela/pkg/utils/addon"
)

const (
	specifyAddonClustersTopologyPolicy = "deploy-addon-to-specified-clusters"
	addonAllClusterPolicy              = "deploy-addon-to-all-clusters"
	renderOutputCuePath                = "output"
	renderAuxiliaryOutputsPath         = "outputs"
	defaultCuePackageHeader            = "main"
)

type addonCueTemplateRender struct {
	addon     *InstallPackage
	inputArgs map[string]interface{}
}

func (a addonCueTemplateRender) formatContext() (string, error) {
	args := a.inputArgs // unify args参数, 防止inputArgs为nil情况
	if args == nil {
		args = map[string]interface{}{}
	}
	bt, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	paramFile := fmt.Sprintf("%s: %s", cuemodel.ParameterFieldName, string(bt))
	// paramFile => parameter: {}
	var contextFile = strings.Builder{}
	/*
		# contextFile 拼接出来的结果如下(对meta.context的value进行了格式化,效果没差别):
		# 这里是不是直接用go template更好一些
		parameter: {
			// +usage=Specify the image hub of velaux, eg. "acr.kubevela.net"
			repo?: string
			// +usage=Specify the database type, current support KubeAPI(default) and MongoDB.
			dbType: *"kubeapi" | "mongodb"
			// +usage=Specify the database name, for the kubeapi db type, it represents namespace.
			database?: string
			// +usage=Specify the MongoDB URL. it only enabled where DB type is MongoDB.
			dbURL?: string
			// +usage=Specify the domain, if set, ingress will be created if the gateway driver is nginx.
			domain?: string
			// +usage=Specify the name of the certificate cecret, if set, means enable the HTTPs.
			secretName?: string
			// +usage=Specify the gateway type.
			gatewayDriver: *"nginx" | "traefik"
			// +usage=Specify the serviceAccountName for apiserver
			serviceAccountName: *"kubevela-vela-core" | string
			// +usage=Specify the service type.
			serviceType: *"ClusterIP" | "NodePort" | "LoadBalancer"
			// +usage=Specify the names of imagePullSecret for private image registry, eg. "{a,b,c}"
			imagePullSecrets?: [...string]
			// +usage=Specify whether to enable the dex
			dex: *false | bool
			// +usage=Specify the replicas.
			replicas: *1 | int
			// +usage=Specify nodeport. This will be ignored if serviceType is not NodePort.
			nodePort: *30000 | int
		}
		context: {
			metadata: {
				"name":"velaux",
				"version":"v1.5.8",
				"description":"KubeVela User Experience (UX). An extensible, application-oriented delivery and management Dashboard.",
				"icon":"https://static.kubevela.net/images/logos/KubeVela%20-03.png",
				"url":"https://kubevela.io",
				"tags":["Official"],
				"deployTo":{
					"disableControlPlane":false,
					"runtimeCluster":false
				},
				"invisible":false,
				"system":{
					"vela":">=v1.5.0"
				}
			}
		}
		// 如果用户未启用自定义参数, 则这里的parameter为空对象
		parameter: {}
		// 如果用户启用了自定义参数, 形式为基础命令+foo=bar的形式(比如vela addon enable velaux serviceType=NodePort a=b), 则这里的parameter为用户自定义的参数
		parameter: {
			"a":"b",
			"serviceType":"NodePort"
		}
	*/
	// user custom parameter but be the first data and generated data should be appended at last
	// in case the user defined data has packages
	contextFile.WriteString(a.addon.Parameters + "\n")

	// addon metadata context
	metadataJSON, err := json.Marshal(a.addon.Meta)
	if err != nil {
		return "", err
	}
	contextFile.WriteString(fmt.Sprintf("context: metadata: %s\n", string(metadataJSON)))
	// parameter definition
	contextFile.WriteString(paramFile + "\n")

	return contextFile.String(), nil
}

// This func can be used for addon render component.
// Please notice the result will be stored in object parameter, so object must be a pointer type
func (a addonCueTemplateRender) toObject(cueTemplate string, path string, object interface{}) error {
	contextFile, err := a.formatContext()
	if err != nil {
		return err
	}
	v, err := value.NewValue(contextFile, nil, "")
	if err != nil {
		return err
	}
	out, err := v.LookupByScript(cueTemplate)
	if err != nil {
		return err
	}
	outputContent, err := out.LookupValue(path)
	if err != nil {
		return err
	}
	return outputContent.UnmarshalTo(object)
}

// renderApp will render Application from CUE files
func (a addonCueTemplateRender) renderApp() (*v1beta1.Application, []*unstructured.Unstructured, error) {
	var app v1beta1.Application
	var outputs = map[string]interface{}{}
	var res []*unstructured.Unstructured
	/*
		cuetextFile 实际上是一个内存中拼接好的字符串
			parameter: {parameter.cue中内容}
			context: {
				meta: {metadata.yaml中内容}
			}
			parameter: {
				// 用户从命令行中传递回来的自定义参数
			}

	*/
	contextFile, err := a.formatContext()
	if err != nil {
		return nil, nil, errors.Wrap(err, "format context for app render")
	}
	contextCue, err := parser.ParseFile("parameter.cue", contextFile, parser.ParseComments)
	if err != nil {
		return nil, nil, errors.Wrap(err, "parse parameter context")
	}
	if contextCue.PackageName() == "" {
		// contextFile 如没有没有 package name, 则默认在开头补上 package main
		contextFile = value.DefaultPackageHeader + contextFile
	}

	var files = []string{contextFile}
	for _, cuef := range a.addon.CUETemplates {
		files = append(files, cuef.Data)
	}

	// TODO(wonderflow): add package discover to support vela own packages if needed
	v, err := value.NewValueWithMainAndFiles(a.addon.AppCueTemplate.Data, files, nil, "")
	if err != nil {
		return nil, nil, errors.Wrap(err, "load app template with CUE files")
	}
	// v.LookupValue("output")
	outputContent, err := v.LookupValue(renderOutputCuePath)
	if err != nil {
		return nil, nil, errors.Wrap(err, "render app from output field from CUE")
	}
	err = outputContent.UnmarshalTo(&app)
	if err != nil {
		return nil, nil, errors.Wrap(err, "decode app from CUE")
	}
	// v.LookupValue("outputs")
	auxiliaryContent, err := v.LookupValue(renderAuxiliaryOutputsPath)
	if err != nil {
		// no outputs defined in app template, return normal data
		if isErrorCueRenderPathNotFound(err, renderAuxiliaryOutputsPath) {
			return &app, res, nil
		}
		return nil, nil, errors.Wrap(err, "render app from output field from CUE")
	}

	err = auxiliaryContent.UnmarshalTo(&outputs)
	if err != nil {
		return nil, nil, errors.Wrap(err, "decode app from CUE")
	}
	for k, o := range outputs {
		if ao, ok := o.(map[string]interface{}); ok {
			auxO := &unstructured.Unstructured{Object: ao}
			auxO.SetLabels(util.MergeMapOverrideWithDst(auxO.GetLabels(), map[string]string{oam.LabelAddonAuxiliaryName: k}))
			res = append(res, auxO)
		}
	}
	return &app, res, nil
}

// generateAppFramework generate application from yaml defined by template.yaml or cue file from template.cue
func generateAppFramework(addon *InstallPackage, parameters map[string]interface{}) (*v1beta1.Application, []*unstructured.Unstructured, error) {
	if len(addon.AppCueTemplate.Data) != 0 && addon.AppTemplate != nil { // 不能同时支持cue和yaml梁总类型的模板
		return nil, nil, ErrBothCueAndYamlTmpl
	}

	var app *v1beta1.Application
	var auxiliaryObjects []*unstructured.Unstructured
	var err error
	if len(addon.AppCueTemplate.Data) != 0 { // 走cue模板渲染
		app, auxiliaryObjects, err = renderAppAccordingToCueTemplate(addon, parameters)
		if err != nil {
			return nil, nil, err
		}
	} else {
		app = addon.AppTemplate
		if app == nil {
			app = &v1beta1.Application{
				TypeMeta: metav1.TypeMeta{APIVersion: v1beta1.SchemeGroupVersion.String(), Kind: v1beta1.ApplicationKind},
			}
		}
		if app.Spec.Components == nil {
			app.Spec.Components = []common2.ApplicationComponent{}
		}
	}

	if app.Name != "" && app.Name != addonutil.Addon2AppName(addon.Name) {
		klog.Warningf("Application name %s will be overwritten with %s. Consider removing metadata.name in template.", app.Name, addonutil.Addon2AppName(addon.Name))
	}
	app.SetName(addonutil.Addon2AppName(addon.Name))

	if app.Namespace != "" && app.Namespace != types.DefaultKubeVelaNS {
		klog.Warningf("Namespace %s will be overwritten with %s. Consider removing metadata.namespace in template.", app.Namespace, types.DefaultKubeVelaNS)
	}
	// force override the namespace defined vela with DefaultVelaNS. This value can be modified by env
	app.SetNamespace(types.DefaultKubeVelaNS)

	if app.Labels == nil {
		app.Labels = make(map[string]string)
	}
	app.Labels[oam.LabelAddonName] = addon.Name       // velaux
	app.Labels[oam.LabelAddonVersion] = addon.Version // 1.5.8

	for _, aux := range auxiliaryObjects {
		aux.SetLabels(util.MergeMapOverrideWithDst(aux.GetLabels(), map[string]string{oam.LabelAddonName: addon.Name, oam.LabelAddonVersion: addon.Version}))
	}

	return app, auxiliaryObjects, nil
}

func renderAppAccordingToCueTemplate(addon *InstallPackage, args map[string]interface{}) (*v1beta1.Application, []*unstructured.Unstructured, error) {
	r := addonCueTemplateRender{ // 构造render
		addon:     addon,
		inputArgs: args,
	}
	return r.renderApp() // 调用renderApp，渲染cue模板
}

// renderCompAccordingCUETemplate will return a component from cue template
func renderCompAccordingCUETemplate(cueTemplate ElementFile, addon *InstallPackage, args map[string]interface{}) (*common2.ApplicationComponent, error) {
	comp := common2.ApplicationComponent{}

	r := addonCueTemplateRender{
		addon:     addon,
		inputArgs: args,
	}
	if err := r.toObject(cueTemplate.Data, renderOutputCuePath, &comp); err != nil {
		return nil, fmt.Errorf("error rendering file %s: %w", cueTemplate.Name, err)
	}
	// If the name of component has been set, just keep it, otherwise will set with file name.
	if len(comp.Name) == 0 {
		fileName := strings.ReplaceAll(cueTemplate.Name, path.Ext(cueTemplate.Name), "")
		comp.Name = strings.ReplaceAll(fileName, ".", "-")
	}
	return &comp, nil
}

// RenderApp render a K8s application
func RenderApp(ctx context.Context, addon *InstallPackage, k8sClient client.Client, args map[string]interface{}) (*v1beta1.Application, []*unstructured.Unstructured, error) {
	// unify args 字段
	if args == nil {
		args = map[string]interface{}{}
	}
	app, auxiliaryObjects, err := generateAppFramework(addon, args)
	if err != nil {
		return nil, nil, err
	}
	// 如果addon中标记了needNamespace，会把创建ns的yaml内容以unstructured.Unstructured形式插入到app.Spec.Components中
	app.Spec.Components = append(app.Spec.Components, renderNeededNamespaceAsComps(addon)...)

	resources, err := renderResources(addon, args)
	if err != nil {
		return nil, nil, err
	}
	app.Spec.Components = append(app.Spec.Components, resources...)

	// for legacy addons those hasn't define policy in template.cue but still want to deploy runtime cluster
	// attach topology policy to application.
	if checkNeedAttachTopologyPolicy(app, addon) {
		if err := attachPolicyForLegacyAddon(ctx, app, addon, args, k8sClient); err != nil {
			return nil, nil, err
		}
	}
	return app, auxiliaryObjects, nil
}

func attachPolicyForLegacyAddon(ctx context.Context, app *v1beta1.Application, addon *InstallPackage, args map[string]interface{}, k8sClient client.Client) error {
	deployClusters, err := checkDeployClusters(ctx, k8sClient, args)
	if err != nil {
		return err
	}

	if !isDeployToRuntime(addon) {
		return nil
	}

	if len(deployClusters) == 0 {
		// empty cluster args deploy to all clusters
		clusterSelector := map[string]interface{}{
			// empty labelSelector means deploy resources to all clusters
			ClusterLabelSelector: map[string]string{},
		}
		properties, err := json.Marshal(clusterSelector)
		if err != nil {
			return err
		}
		policy := v1beta1.AppPolicy{
			Name:       addonAllClusterPolicy,
			Type:       v1alpha1.TopologyPolicyType,
			Properties: &runtime.RawExtension{Raw: properties},
		}
		app.Spec.Policies = append(app.Spec.Policies, policy)
	} else {
		var found bool
		for _, c := range deployClusters {
			if c == multicluster.ClusterLocalName {
				found = true
				break
			}
		}
		if !found {
			deployClusters = append(deployClusters, multicluster.ClusterLocalName)
		}
		// deploy to specified clusters
		if app.Spec.Policies == nil {
			app.Spec.Policies = []v1beta1.AppPolicy{}
		}
		body, err := json.Marshal(map[string][]string{types.ClustersArg: deployClusters})
		if err != nil {
			return err
		}
		app.Spec.Policies = append(app.Spec.Policies, v1beta1.AppPolicy{
			Name:       specifyAddonClustersTopologyPolicy,
			Type:       v1alpha1.TopologyPolicyType,
			Properties: &runtime.RawExtension{Raw: body},
		})
	}

	return nil
}

func renderResources(addon *InstallPackage, args map[string]interface{}) ([]common2.ApplicationComponent, error) {
	var resources []common2.ApplicationComponent
	if len(addon.YAMLTemplates) != 0 {
		comp, err := renderK8sObjectsComponent(addon.YAMLTemplates, addon.Name)
		if err != nil {
			return nil, errors.Wrapf(err, "render components from yaml template")
		}
		resources = append(resources, *comp)
	}

	for _, tmpl := range addon.CUETemplates {
		isMainCueTemplate, err := checkCueFileHasPackageHeader(tmpl)
		if err != nil {
			return nil, err
		}
		if isMainCueTemplate {
			continue
		}
		comp, err := renderCompAccordingCUETemplate(tmpl, addon, args)
		if err != nil && strings.Contains(err.Error(), "var(path=output) not exist") {
			continue
		}
		if err != nil {
			return nil, NewAddonError(fmt.Sprintf("fail to render cue template %s", err.Error()))
		}
		resources = append(resources, *comp)
	}
	return resources, nil
}

// checkNeedAttachTopologyPolicy will check this addon want to deploy to runtime-cluster, but application template doesn't specify the
// topology policy, then will attach the policy to application automatically.
func checkNeedAttachTopologyPolicy(app *v1beta1.Application, addon *InstallPackage) bool {
	if !isDeployToRuntime(addon) {
		return false
	}
	for _, policy := range app.Spec.Policies {
		if policy.Type == v1alpha1.TopologyPolicyType {
			klog.Warningf("deployTo in metadata will NOT have any effect. It conflicts with %s policy named %s. Consider removing deployTo field in addon metadata.", v1alpha1.TopologyPolicyType, policy.Name)
			return false
		}
	}
	return true
}

func isDeployToRuntime(addon *InstallPackage) bool {
	if addon.DeployTo == nil {
		return false
	}
	return addon.DeployTo.RuntimeCluster || addon.DeployTo.LegacyRuntimeCluster
}

func checkCueFileHasPackageHeader(cueTemplate ElementFile) (bool, error) {
	cueFile, err := parser.ParseFile(cueTemplate.Name, cueTemplate.Data, parser.ParseComments)
	if err != nil {
		return false, err
	}
	if cueFile.PackageName() == defaultCuePackageHeader {
		return true, nil
	}
	return false, nil
}

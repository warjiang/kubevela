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

package addon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"text/template"
	"time"

	"cuelang.org/go/cue"
	"github.com/Masterminds/semver/v3"
	"github.com/google/go-github/v32/github"
	"github.com/imdario/mergo"
	prismclusterv1alpha1 "github.com/kubevela/prism/pkg/apis/cluster/v1alpha1"
	"github.com/pkg/errors"
	"github.com/xanzy/go-gitlab"
	"golang.org/x/oauth2"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8syaml "k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	types2 "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	common2 "github.com/oam-dev/kubevela/apis/core.oam.dev/common"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/apis/types"
	"github.com/oam-dev/kubevela/pkg/apiserver/utils/log"
	utils2 "github.com/oam-dev/kubevela/pkg/controller/utils"
	"github.com/oam-dev/kubevela/pkg/definition"
	"github.com/oam-dev/kubevela/pkg/multicluster"
	"github.com/oam-dev/kubevela/pkg/oam"
	"github.com/oam-dev/kubevela/pkg/oam/util"
	"github.com/oam-dev/kubevela/pkg/utils"
	addonutil "github.com/oam-dev/kubevela/pkg/utils/addon"
	"github.com/oam-dev/kubevela/pkg/utils/apply"
	"github.com/oam-dev/kubevela/pkg/utils/common"
	"github.com/oam-dev/kubevela/pkg/velaql"
	version2 "github.com/oam-dev/kubevela/version"
)

const (
	// ReadmeFileName is the addon readme file name
	ReadmeFileName string = "README.md"

	// LegacyReadmeFileName is the addon readme lower case file name
	LegacyReadmeFileName string = "readme.md"

	// MetadataFileName is the addon meatadata.yaml file name
	MetadataFileName string = "metadata.yaml"

	// TemplateFileName is the addon template.yaml file name
	TemplateFileName string = "template.yaml"

	// AppTemplateCueFileName is the addon application template.cue file name
	AppTemplateCueFileName string = "template.cue"

	// GlobalParameterFileName is the addon global parameter.cue file name
	GlobalParameterFileName string = "parameter.cue"

	// ResourcesDirName is the addon resources/ dir name
	ResourcesDirName string = "resources"

	// DefinitionsDirName is the addon definitions/ dir name
	DefinitionsDirName string = "definitions"

	// DefSchemaName is the addon definition schemas dir name
	DefSchemaName string = "schemas"

	// ViewDirName is the addon views dir name
	ViewDirName string = "views"

	// AddonParameterDataKey is the key of parameter in addon args secrets
	AddonParameterDataKey string = "addonParameterDataKey"

	// DefaultGiteeURL is the addon repository of gitee api
	DefaultGiteeURL string = "https://gitee.com/api/v5/"
)

// ParameterFileName is the addon resources/parameter.cue file name
var ParameterFileName = strings.Join([]string{"resources", "parameter.cue"}, "/")

// ListOptions contains flags mark what files should be read in an addon directory
type ListOptions struct {
	GetDetail     bool
	GetDefinition bool
	GetResource   bool
	GetParameter  bool
	GetTemplate   bool
	GetDefSchema  bool
}

var (
	// UIMetaOptions get Addon metadata for UI display
	UIMetaOptions = ListOptions{GetDetail: true, GetDefinition: true, GetParameter: true}

	// CLIMetaOptions get Addon metadata for CLI display
	CLIMetaOptions = ListOptions{}

	// UnInstallOptions used for addon uninstalling
	UnInstallOptions = ListOptions{GetDefinition: true}
)

const (
	// ObservabilityAddon is the name of the observability addon
	ObservabilityAddon = "observability"
	// ObservabilityAddonEndpointComponent is the endpoint component name of the observability addon
	ObservabilityAddonEndpointComponent = "grafana"
	// ObservabilityAddonDomainArg is the domain argument name of the observability addon
	ObservabilityAddonDomainArg = "domain"
	// LocalAddonRegistryName is the addon-registry name for those installed by local dir
	LocalAddonRegistryName = "local"
	// ClusterLabelSelector define the key of topology cluster label selector
	ClusterLabelSelector = "clusterLabelSelector"
)

// ObservabilityEnvironment contains the Observability addon's domain for each cluster
type ObservabilityEnvironment struct {
	Cluster           string
	Domain            string
	LoadBalancerIP    string
	ServiceExternalIP string
}

// ObservabilityEnvBindingValues is a list of ObservabilityEnvironment and will be used to render observability-env-binding.yaml
type ObservabilityEnvBindingValues struct {
	Envs []ObservabilityEnvironment
}

const (
	// ObservabilityEnvBindingEnvTag is the env Tag for env-binding settings for observability addon
	ObservabilityEnvBindingEnvTag = `        envs:`

	// ObservabilityEnvBindingEnvTmpl is the env values for env-binding settings for observability addon
	ObservabilityEnvBindingEnvTmpl = `
        {{ with .Envs}}
          {{ range . }}
          - name: {{.Cluster}}
            placement:
              clusterSelector:
                name: {{.Cluster}}
          {{ end }}
        {{ end }}`

	// ObservabilityWorkflowStepsTag is the workflow steps Tag for observability addon
	ObservabilityWorkflowStepsTag = `steps:`

	// ObservabilityWorkflow4EnvBindingTmpl is the workflow for env-binding settings for observability addon
	ObservabilityWorkflow4EnvBindingTmpl = `
{{ with .Envs}}
  {{ range . }}
  - name: {{ .Cluster }}
    type: deploy2env
    properties:
      policy: domain
      env: {{ .Cluster }}
      parallel: true
  {{ end }}
{{ end }}`
)

// ErrorNoDomain is the error when no domain is found
var ErrorNoDomain = errors.New("domain is not set")

// Pattern indicates the addon framework file pattern, all files should match at least one of the pattern.
type Pattern struct {
	IsDir bool
	Value string
}

// Patterns is the file pattern that the addon should be in
var Patterns = []Pattern{
	{Value: ReadmeFileName},
	{Value: MetadataFileName},
	{Value: TemplateFileName},
	{Value: ParameterFileName},
	{IsDir: true, Value: ResourcesDirName},
	{IsDir: true, Value: DefinitionsDirName},
	{IsDir: true, Value: DefSchemaName},
	{IsDir: true, Value: ViewDirName},
	{Value: AppTemplateCueFileName},
	{Value: GlobalParameterFileName},
	{Value: LegacyReadmeFileName},
}

/*
这里匹配的过程其实也是就是相当于描述了addon的相对目录结构
addons/xx
├── README.md
├── metadata.yaml
├── template.yaml
├── resources/parameter.cue
├── resources
├── definitions
├── schemas
├── views
├── template.cue
├── parameter.cue
├── readme.md
*/

// GetPatternFromItem will check if the file path has a valid pattern, return empty string if it's invalid.
// AsyncReader is needed to calculate relative path
func GetPatternFromItem(it Item, r AsyncReader, rootPath string) string {
	// 这里rootPath有点误导了，其实想表达的是addonName
	// AsyncReader计算RelativePath返回的结果也是按照addonName+子文件相对addon的文件路径的形式
	relativePath := r.RelativePath(it)
	for _, p := range Patterns {
		// 前缀匹配，
		if strings.HasPrefix(relativePath, strings.Join([]string{rootPath, p.Value}, "/")) {
			return p.Value
		}
		if strings.HasPrefix(relativePath, filepath.Join(rootPath, p.Value)) {
			// for enable addon by load dir, compatible with linux or windows os
			return p.Value
		}
	}
	return ""
}

// ListAddonUIDataFromReader list addons from AsyncReader
func ListAddonUIDataFromReader(r AsyncReader, registryMeta map[string]SourceMeta, registryName string, opt ListOptions) ([]*UIData, error) {
	var addons []*UIData
	var err error
	var wg sync.WaitGroup
	var errs []error
	errCh := make(chan error)
	waitCh := make(chan struct{})

	var l sync.Mutex
	for _, subItem := range registryMeta {
		wg.Add(1)
		go func(addonMeta SourceMeta) {
			defer wg.Done()
			addonRes, err := GetUIDataFromReader(r, &addonMeta, opt)
			if err != nil {
				errCh <- err
				return
			}
			addonRes.RegistryName = registryName
			l.Lock()
			addons = append(addons, addonRes)
			l.Unlock()
		}(subItem)
	}
	// in another goroutine for wait group to finish
	go func() {
		wg.Wait()
		close(waitCh)
	}()
forLoop:
	for {
		select {
		case <-waitCh:
			break forLoop
		case err = <-errCh:
			errs = append(errs, err)
		}
	}
	if len(errs) != 0 {
		return addons, compactErrors("error(s) happen when reading from registry: ", errs)
	}
	return addons, nil
}

func compactErrors(message string, errs []error) error {
	errForPrint := make([]string, 0)
	for _, e := range errs {
		errForPrint = append(errForPrint, e.Error())
	}

	return errors.New(message + strings.Join(errForPrint, ","))

}

// GetUIDataFromReader read ui metadata of addon from Reader, used to be displayed in UI
func GetUIDataFromReader(r AsyncReader, meta *SourceMeta, opt ListOptions) (*UIData, error) {
	addonContentsReader := map[string]struct {
		skip bool
		read func(a *UIData, reader AsyncReader, readPath string) error
	}{
		ReadmeFileName:          {!opt.GetDetail, readReadme},
		LegacyReadmeFileName:    {!opt.GetDetail, readReadme},
		MetadataFileName:        {false, readMetadata},
		DefinitionsDirName:      {!opt.GetDefinition, readDefFile},
		ParameterFileName:       {!opt.GetParameter, readParamFile},
		GlobalParameterFileName: {!opt.GetParameter, readGlobalParamFile},
	}
	// 对metas下的所有文件进行分类，结果会和addonContentsReader对齐
	ptItems := ClassifyItemByPattern(meta, r)
	var addon = &UIData{}
	/*
		addonContentsReader保存了文件名和对应处理函数的映射关系
			README.md => readReadme 函数, opt.GetDetail为false时忽略
			readme.md => readReadme 函数, opt.GetDetail为false时忽略
			metadata.yaml => readMetadata 函数, 必须执行，无法忽略
			definitions => redDefFile 函数, opt.GetDefinition为false时忽略
			resources/parameter.cue => readParamFile 函数, opt.GetParameter为false时忽略
			parameter.cue => readGlobalParamFile 函数, opt.GetParameter为false时忽略
	*/
	// 遍历meta列表, 根据文件名调用对应的处理函数
	for contentType, method := range addonContentsReader {
		if method.skip {
			continue
		}
		items := ptItems[contentType]
		for _, it := range items {
			err := method.read(addon, r, r.RelativePath(it))
			if err != nil {
				return nil, fmt.Errorf("fail to read addon %s file %s: %w", meta.Name, r.RelativePath(it), err)
			}
		}
	}

	if opt.GetParameter && (len(addon.Parameters) != 0 || len(addon.GlobalParameters) != 0) {
		if addon.GlobalParameters != "" {
			if addon.Parameters != "" {
				klog.Warning("both legacy parameter and global parameter are provided, but only global parameter will be used. Consider removing the legacy parameters.")
			}
			addon.Parameters = addon.GlobalParameters
		}
		err := genAddonAPISchema(addon)
		if err != nil {
			return nil, fmt.Errorf("fail to generate openAPIschema for addon %s : %w", meta.Name, err)
		}
	}
	addon.AvailableVersions = []string{addon.Version}
	return addon, nil
}

// GetInstallPackageFromReader get install package of addon from Reader, this is used to enable an addon
func GetInstallPackageFromReader(r AsyncReader, meta *SourceMeta, uiData *UIData) (*InstallPackage, error) {
	addonContentsReader := map[string]func(a *InstallPackage, reader AsyncReader, readPath string) error{
		TemplateFileName:       readTemplate,
		ResourcesDirName:       readResFile,
		DefSchemaName:          readDefSchemaFile,
		ViewDirName:            readViewFile,
		AppTemplateCueFileName: readAppCueTemplate,
	}
	/*
		template.yaml => readTemplate 函数
		resources => readResFile 函数
		schemas => readDefSchemaFile 函数
		views => readViewFile 函数
		template.cue => readAppCueTemplate 函数
	*/
	ptItems := ClassifyItemByPattern(meta, r)

	// Read the installed data from UI metadata object to reduce network payload
	var addon = &InstallPackage{
		Meta:           uiData.Meta,
		Definitions:    uiData.Definitions,
		CUEDefinitions: uiData.CUEDefinitions,
		Parameters:     uiData.Parameters,
	}

	for contentType, method := range addonContentsReader {
		items := ptItems[contentType]
		for _, it := range items {
			err := method(addon, r, r.RelativePath(it))
			if err != nil {
				return nil, fmt.Errorf("fail to read addon %s file %s: %w", meta.Name, r.RelativePath(it), err)
			}
		}
	}

	return addon, nil
}

func readTemplate(a *InstallPackage, reader AsyncReader, readPath string) error {
	// 读取 template.cue 文件，
	// 生成 unstructured.UnstructuredJSONScheme 反序列化函数 dec
	// 调用 dec 反序列化文件内容到 InstallPackage.AppTemplate
	data, err := reader.ReadFile(readPath)
	if err != nil {
		return err
	}
	dec := k8syaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)
	a.AppTemplate = &v1beta1.Application{}

	// try to check it's a valid app template
	_, _, err = dec.Decode([]byte(data), nil, a.AppTemplate)
	if err != nil {
		return err
	}
	return nil
}

func readAppCueTemplate(a *InstallPackage, reader AsyncReader, readPath string) error {
	data, err := reader.ReadFile(readPath)
	if err != nil {
		return err
	}
	a.AppCueTemplate = ElementFile{Data: data, Name: filepath.Base(readPath)}
	return nil
}

// readParamFile read single resource/parameter.cue file
func readParamFile(a *UIData, reader AsyncReader, readPath string) error {
	// resource/parameter.cue 读取到 UIData.Parameters
	b, err := reader.ReadFile(readPath)
	if err != nil {
		return err
	}
	a.Parameters = b
	return nil
}

// readGlobalParamFile read global parameter file.
func readGlobalParamFile(a *UIData, reader AsyncReader, readPath string) error {
	// 读取parameter.cue到UIData.GlobalParameters中
	b, err := reader.ReadFile(readPath)
	if err != nil {
		return err
	}
	a.GlobalParameters = b
	return nil
}

// readResFile read single resource file
func readResFile(a *InstallPackage, reader AsyncReader, readPath string) error {
	filename := path.Base(readPath)
	b, err := reader.ReadFile(readPath)
	if err != nil {
		return err
	}

	if filename == "parameter.cue" {
		return nil
	}
	file := ElementFile{Data: b, Name: filepath.Base(readPath)}
	switch filepath.Ext(filename) {
	case ".cue":
		a.CUETemplates = append(a.CUETemplates, file)
	case ".yaml", ".yml":
		a.YAMLTemplates = append(a.YAMLTemplates, file)
	default:
		// skip other file formats
	}
	return nil
}

// readDefSchemaFile read single file of definition schema
func readDefSchemaFile(a *InstallPackage, reader AsyncReader, readPath string) error {
	b, err := reader.ReadFile(readPath)
	if err != nil {
		return err
	}
	a.DefSchemas = append(a.DefSchemas, ElementFile{Data: b, Name: filepath.Base(readPath)})
	return nil
}

// readDefFile read single definition file
func readDefFile(a *UIData, reader AsyncReader, readPath string) error {
	b, err := reader.ReadFile(readPath)
	if err != nil {
		return err
	}
	filename := path.Base(readPath)
	file := ElementFile{Data: b, Name: filepath.Base(readPath)}
	// 根据文件名的后缀分类读取,
	// cue文件读取到CUEDefinitions, yaml/yml文件读取到Definitions
	switch filepath.Ext(filename) {
	case ".cue":
		a.CUEDefinitions = append(a.CUEDefinitions, file)
	case ".yaml", ".yml":
		a.Definitions = append(a.Definitions, file)
	default:
		// skip other file formats
	}
	return nil
}

// readViewFile read single view file
func readViewFile(a *InstallPackage, reader AsyncReader, readPath string) error {
	b, err := reader.ReadFile(readPath)
	if err != nil {
		return err
	}
	filename := path.Base(readPath)
	switch filepath.Ext(filename) {
	case ".cue":
		a.CUEViews = append(a.CUEViews, ElementFile{Data: b, Name: filepath.Base(readPath)})
	case ".yaml", ".yml":
		a.YAMLViews = append(a.YAMLViews, ElementFile{Data: b, Name: filepath.Base(readPath)})
	default:
		// skip other file formats
	}
	return nil
}

func readMetadata(a *UIData, reader AsyncReader, readPath string) error {
	b, err := reader.ReadFile(readPath)
	if err != nil {
		return err
	}
	// 读文件，反序列化成 addon.Meta 类型的数据存储到 UIData.Meta 中
	err = yaml.Unmarshal([]byte(b), &a.Meta)
	if err != nil {
		return err
	}
	return nil
}

func readReadme(a *UIData, reader AsyncReader, readPath string) error {
	// the detail will contain readme.md or README.md, if the content already is filled, don't read another.
	if len(a.Detail) != 0 {
		return nil
	}
	content, err := reader.ReadFile(readPath)
	if err != nil {
		return err
	}
	// readme 最终结果会以 string 形式塞到UIData.Detail字段中
	a.Detail = content
	return nil
}

func createGitHelper(content *utils.Content, token string) *gitHelper {
	var ts oauth2.TokenSource
	if token != "" {
		ts = oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	}
	tc := oauth2.NewClient(context.Background(), ts)
	tc.Timeout = time.Second * 20
	cli := github.NewClient(tc)
	return &gitHelper{
		Client: cli,
		Meta:   content,
	}
}

func createGiteeHelper(content *utils.Content, token string) *giteeHelper {
	var ts oauth2.TokenSource
	if token != "" {
		ts = oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	}
	tc := oauth2.NewClient(context.Background(), ts)
	tc.Timeout = time.Second * 20
	cli := NewGiteeClient(tc, nil)
	return &giteeHelper{
		Client: cli,
		Meta:   content,
	}
}

func createGitlabHelper(content *utils.Content, token string) (*gitlabHelper, error) {
	newClient, err := gitlab.NewClient(token, gitlab.WithBaseURL(content.GitlabContent.Host))

	return &gitlabHelper{
		Client: newClient,
		Meta:   content,
	}, err
}

// readRepo will read relative path (relative to Meta.Path)
func (h *gitHelper) readRepo(relativePath string) (*github.RepositoryContent, []*github.RepositoryContent, error) {
	file, items, _, err := h.Client.Repositories.GetContents(context.Background(), h.Meta.GithubContent.Owner, h.Meta.GithubContent.Repo, path.Join(h.Meta.GithubContent.Path, relativePath), nil)
	if err != nil {
		return nil, nil, WrapErrRateLimit(err)
	}
	return file, items, nil
}

// readRepo will read relative path (relative to Meta.Path)
func (h *giteeHelper) readRepo(relativePath string) (*github.RepositoryContent, []*github.RepositoryContent, error) {
	file, items, err := h.Client.GetGiteeContents(context.Background(), h.Meta.GiteeContent.Owner, h.Meta.GiteeContent.Repo, path.Join(h.Meta.GiteeContent.Path, relativePath), h.Meta.GiteeContent.Ref)
	if err != nil {
		return nil, nil, WrapErrRateLimit(err)
	}
	return file, items, nil
}

// GetGiteeContents can return either the metadata and content of a single file
func (c *Client) GetGiteeContents(ctx context.Context, owner, repo, path, ref string) (fileContent *github.RepositoryContent, directoryContent []*github.RepositoryContent, err error) {
	escapedPath := (&url.URL{Path: path}).String()
	u := fmt.Sprintf(c.BaseURL.String()+"repos/%s/%s/contents/%s", owner, repo, escapedPath)
	if ref != "" {
		u = fmt.Sprintf(u+"?ref=%s", ref)
	}

	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, nil, err
	}
	response, err := c.Client.Do(req.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	//nolint:errcheck
	defer response.Body.Close()
	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return nil, nil, err
	}
	return unmarshalToContent(body)
}

func unmarshalToContent(content []byte) (fileContent *github.RepositoryContent, directoryContent []*github.RepositoryContent, err error) {
	fileUnmarshalError := json.Unmarshal(content, &fileContent)
	if fileUnmarshalError == nil {
		return fileContent, nil, nil
	}
	directoryUnmarshalError := json.Unmarshal(content, &directoryContent)
	if directoryUnmarshalError == nil {
		return nil, directoryContent, nil
	}
	return nil, nil, fmt.Errorf("unmarshalling failed for both file and directory content: %s and %w", fileUnmarshalError, directoryUnmarshalError)
}

func genAddonAPISchema(addonRes *UIData) error {
	param, err := utils2.PrepareParameterCue(addonRes.Name, addonRes.Parameters)
	if err != nil {
		return err
	}
	var r cue.Runtime
	cueInst, err := r.Compile("-", param)
	if err != nil {
		return err
	}
	data, err := common.GenOpenAPI(cueInst)
	if err != nil {
		return err
	}
	schema, err := utils2.ConvertOpenAPISchema2SwaggerObject(data)
	if err != nil {
		return err
	}
	utils2.FixOpenAPISchema("", schema)
	addonRes.APISchema = schema
	return nil
}

func getClusters(args map[string]interface{}) []string {
	ccr, ok := args[types.ClustersArg]
	if !ok {
		return nil
	}
	cc, ok := ccr.([]string)
	if !ok {
		return nil
	}
	return cc
}

// renderNeededNamespaceAsComps will convert namespace as app components to create namespace for managed clusters
func renderNeededNamespaceAsComps(addon *InstallPackage) []common2.ApplicationComponent {
	var nscomps []common2.ApplicationComponent
	// create namespace for managed clusters
	for _, namespace := range addon.NeedNamespace {
		// vela-system must exist before rendering vela addon
		if namespace == types.DefaultKubeVelaNS {
			continue
		}
		comp := common2.ApplicationComponent{
			Type:       "raw",
			Name:       fmt.Sprintf("%s-namespace", namespace),
			Properties: util.Object2RawExtension(renderNamespace(namespace)),
		}
		nscomps = append(nscomps, comp)
	}
	return nscomps
}

func checkDeployClusters(ctx context.Context, k8sClient client.Client, args map[string]interface{}) ([]string, error) {
	deployClusters := getClusters(args)
	if len(deployClusters) == 0 || k8sClient == nil {
		return nil, nil
	}

	clusters, err := prismclusterv1alpha1.NewClusterClient(k8sClient).List(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "fail to get registered cluster")
	}

	clusterNames := sets.String{}
	if len(clusters.Items) != 0 {
		for _, cluster := range clusters.Items {
			clusterNames.Insert(cluster.Name)
		}
	}

	var res []string
	for _, c := range deployClusters {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if !clusterNames.Has(c) {
			return nil, errors.Errorf("cluster %s not exist", c)
		}
		res = append(res, c)
	}
	return res, nil
}

// RenderDefinitions render definition objects if needed
func RenderDefinitions(addon *InstallPackage, config *rest.Config) ([]*unstructured.Unstructured, error) {
	defObjs := make([]*unstructured.Unstructured, 0)

	// No matter runtime mode or control mode, definition only needs to control plane k8s.
	for _, def := range addon.Definitions {
		obj, err := renderObject(def)
		if err != nil {
			return nil, errors.Wrapf(err, "render definition file %s", def.Name)
		}
		// we should ignore the namespace defined in definition yaml, override the filed by DefaultKubeVelaNS
		obj.SetNamespace(types.DefaultKubeVelaNS)
		defObjs = append(defObjs, obj)
	}

	for _, cueDef := range addon.CUEDefinitions {
		def := definition.Definition{Unstructured: unstructured.Unstructured{}}
		err := def.FromCUEString(cueDef.Data, config)
		if err != nil {
			return nil, errors.Wrapf(err, "fail to render definition: %s in cue's format", cueDef.Name)
		}
		// we should ignore the namespace defined in definition yaml, override the filed by DefaultKubeVelaNS
		def.SetNamespace(types.DefaultKubeVelaNS)
		defObjs = append(defObjs, &def.Unstructured)
	}

	return defObjs, nil
}

// RenderDefinitionSchema will render definitions' schema in addons.
func RenderDefinitionSchema(addon *InstallPackage) ([]*unstructured.Unstructured, error) {
	schemaConfigmaps := make([]*unstructured.Unstructured, 0)

	// No matter runtime mode or control mode , definition schemas only needs to control plane k8s.
	for _, teml := range addon.DefSchemas {
		u, err := renderSchemaConfigmap(teml)
		if err != nil {
			return nil, errors.Wrapf(err, "render uiSchema file %s", teml.Name)
		}
		schemaConfigmaps = append(schemaConfigmaps, u)
	}
	return schemaConfigmaps, nil
}

// RenderViews will render views in addons.
func RenderViews(addon *InstallPackage) ([]*unstructured.Unstructured, error) {
	views := make([]*unstructured.Unstructured, 0)
	for _, view := range addon.YAMLViews {
		obj, err := renderObject(view)
		if err != nil {
			return nil, errors.Wrapf(err, "render velaQL view file %s", view.Name)
		}
		views = append(views, obj)
	}
	for _, view := range addon.CUEViews {
		obj, err := renderCUEView(view)
		if err != nil {
			return nil, errors.Wrapf(err, "render velaQL view file %s", view.Name)
		}
		views = append(views, obj)
	}
	return views, nil
}

func allocateDomainForAddon(ctx context.Context, k8sClient client.Client) ([]ObservabilityEnvironment, error) {
	secrets, err := multicluster.ListExistingClusterSecrets(ctx, k8sClient)
	if err != nil {
		klog.Error(err, "failed to list existing cluster secrets")
		return nil, err
	}

	envs := make([]ObservabilityEnvironment, len(secrets))

	for i, secret := range secrets {
		cluster := secret.Name
		envs[i] = ObservabilityEnvironment{
			Cluster: cluster,
		}
	}

	return envs, nil
}

func render(envs []ObservabilityEnvironment, tmpl string) (string, error) {
	todos := ObservabilityEnvBindingValues{
		Envs: envs,
	}

	t := template.Must(template.New("grafana").Parse(tmpl))
	var rendered bytes.Buffer
	err := t.Execute(&rendered, todos)
	if err != nil {
		return "", err
	}

	return rendered.String(), nil
}

func renderObject(elem ElementFile) (*unstructured.Unstructured, error) {
	obj := &unstructured.Unstructured{}
	dec := k8syaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)
	_, _, err := dec.Decode([]byte(elem.Data), nil, obj)
	if err != nil {
		return nil, err
	}
	return obj, nil
}

func renderNamespace(namespace string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("v1")
	u.SetKind("Namespace")
	u.SetName(namespace)
	return u
}

func renderK8sObjectsComponent(elems []ElementFile, addonName string) (*common2.ApplicationComponent, error) {
	var objects []*unstructured.Unstructured
	for _, elem := range elems {
		obj, err := renderObject(elem)
		if err != nil {
			return nil, errors.Wrapf(err, "render resource file %s", elem.Name)
		}
		objects = append(objects, obj)
	}
	properties := map[string]interface{}{"objects": objects}
	propJSON, err := json.Marshal(properties)
	if err != nil {
		return nil, err
	}
	baseRawComponent := common2.ApplicationComponent{
		Type:       "k8s-objects",
		Name:       addonName + "-resources",
		Properties: &runtime.RawExtension{Raw: propJSON},
	}
	return &baseRawComponent, nil
}

func renderSchemaConfigmap(elem ElementFile) (*unstructured.Unstructured, error) {
	jsonData, err := yaml.YAMLToJSON([]byte(elem.Data))
	if err != nil {
		return nil, err
	}
	cm := v1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Namespace: types.DefaultKubeVelaNS, Name: strings.Split(elem.Name, ".")[0]},
		Data: map[string]string{
			types.UISchema: string(jsonData),
		}}
	return util.Object2Unstructured(cm)
}

func renderCUEView(elem ElementFile) (*unstructured.Unstructured, error) {
	name, err := utils.GetFilenameFromLocalOrRemote(elem.Name)
	if err != nil {
		return nil, err
	}

	cm, err := velaql.ParseViewIntoConfigMap(elem.Data, name)
	if err != nil {
		return nil, err
	}

	return util.Object2Unstructured(*cm)
}

// RenderArgsSecret render addon enable argument to secret
func RenderArgsSecret(addon *InstallPackage, args map[string]interface{}) *unstructured.Unstructured {
	argsByte, err := json.Marshal(args)
	if err != nil {
		return nil
	}
	sec := v1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      addonutil.Addon2SecName(addon.Name),
			Namespace: types.DefaultKubeVelaNS,
		},
		Data: map[string][]byte{
			AddonParameterDataKey: argsByte,
		},
		Type: v1.SecretTypeOpaque,
	}
	u, err := util.Object2Unstructured(sec)
	if err != nil {
		return nil
	}
	return u
}

// FetchArgsFromSecret fetch addon args from secrets
func FetchArgsFromSecret(sec *v1.Secret) (map[string]interface{}, error) {
	// addon的参数以secret形式存储
	// 新版本的secret可以为addonParameterDataKey, 值为具体的参数
	// 旧版本的secret直接按照k-v的形式存储在secret中
	res := map[string]interface{}{}
	if args, ok := sec.Data[AddonParameterDataKey]; ok {
		err := json.Unmarshal(args, &res)
		if err != nil {
			return nil, err
		}
		return res, nil
	}

	// this is backward compatibility code for old way to storage parameter
	res = make(map[string]interface{}, len(sec.Data))
	for k, v := range sec.Data {
		res[k] = string(v)
	}
	return res, nil
}

// Installer helps addon enable, dependency-check, dispatch resources
type Installer struct {
	ctx                 context.Context
	config              *rest.Config
	addon               *InstallPackage
	cli                 client.Client
	apply               apply.Applicator
	r                   *Registry
	registryMeta        map[string]SourceMeta
	args                map[string]interface{}
	cache               *Cache
	dc                  *discovery.DiscoveryClient
	skipVersionValidate bool
	overrideDefs        bool
}

// NewAddonInstaller will create an installer for addon
func NewAddonInstaller(ctx context.Context, cli client.Client, discoveryClient *discovery.DiscoveryClient, apply apply.Applicator, config *rest.Config, r *Registry, args map[string]interface{}, cache *Cache, opts ...InstallOption) Installer {
	i := Installer{
		ctx:    ctx,
		config: config,
		cli:    cli,
		apply:  apply,
		r:      r,
		args:   args,
		cache:  cache,
		dc:     discoveryClient,
	}
	for _, opt := range opts {
		opt(&i)
	}
	return i
}

func (h *Installer) enableAddon(addon *InstallPackage) error {
	// enable addon 核心逻辑
	var err error
	h.addon = addon

	if !h.skipVersionValidate {
		// 检查 vela-cli(本地)、vela-core(远端)、kubernetes(远端) 版本
		// addon -> system 检查失败 addon.SystemRequirements{VelaVersion:">=v1.5.0", KubernetesVersion:"" }
		err = checkAddonVersionMeetRequired(h.ctx, addon.SystemRequirements, h.cli, h.dc)
		if err != nil {
			// 尝试获取满足系统要求最新的addon verison
			version := h.getAddonVersionMeetSystemRequirement(addon.Name)
			return VersionUnMatchError{addonName: addon.Name, err: err, userSelectedAddonVersion: addon.Version, availableVersion: version}
		}
	}
	// 安装 addon 依赖
	if err = h.installDependency(addon); err != nil {
		return err
	}
	//
	if err = h.dispatchAddonResource(addon); err != nil {
		return err
	}
	// we shouldn't put continue func into dispatchAddonResource, because the re-apply app maybe already update app and
	// the suspend will set with false automatically
	if err := h.continueOrRestartWorkflow(); err != nil {
		return err
	}
	return nil
}

func (h *Installer) loadInstallPackage(name, version string) (*InstallPackage, error) {
	// 如果version为空则表示使用最新版本
	var installPackage *InstallPackage
	var err error
	if !IsVersionRegistry(*h.r) {
		metas, err := h.getAddonMeta()
		if err != nil {
			return nil, errors.Wrap(err, "fail to get addon meta")
		}

		meta, ok := metas[name]
		if !ok {
			return nil, ErrNotExist
		}
		var uiData *UIData
		uiData, err = h.cache.GetUIData(*h.r, name, version)
		if err != nil {
			return nil, err
		}
		// enable this addon if it's invisible
		installPackage, err = h.r.GetInstallPackage(&meta, uiData)
		if err != nil {
			return nil, errors.Wrap(err, "fail to find dependent addon in source repository")
		}
	} else {
		versionedRegistry := BuildVersionedRegistry(h.r.Name, h.r.Helm.URL, &common.HTTPOption{
			Username:        h.r.Helm.Username,
			Password:        h.r.Helm.Password,
			InsecureSkipTLS: h.r.Helm.InsecureSkipTLS,
		})
		installPackage, err = versionedRegistry.GetAddonInstallPackage(context.Background(), name, version)
		if err != nil {
			return nil, err
		}
	}

	return installPackage, nil
}

func (h *Installer) getAddonMeta() (map[string]SourceMeta, error) {
	var err error
	if h.registryMeta == nil {
		if h.registryMeta, err = h.cache.ListAddonMeta(*h.r); err != nil {
			return nil, err
		}
	}
	return h.registryMeta, nil
}

// installDependency checks if addon's dependency and install it
func (h *Installer) installDependency(addon *InstallPackage) error {
	/*
		{
		  "name": "velaux",
		  "version": "v1.5.8",
		  "description": "KubeVela User Experience (UX). An extensible, application-oriented delivery and management Dashboard.",
		  "icon": "https://static.kubevela.net/images/logos/KubeVela%20-03.png",
		  "url": "https://kubevela.io",
		  "tags": [
		    "Official"
		  ],
		  "deployTo": {
		    "disableControlPlane": false,
		    "runtimeCluster": false
		  },
		  "invisible": false,
		  "system": {
		    "vela": ">=v1.5.0"
		  },
		  "definitions": null,
		  "CUEDefinitions": null,
		  "YAMLViews": null,
		  "CUEViews": [
		    {
		      "Data": "{service-view.cue文件对应内容}",
		      "Name": "service-view.cue"
		    },
		    {
		      "Data": "{application-resource-detail-view.cue文件对应内容}",
		      "Name": "application-resource-detail-view.cue"
		    },
		    {
		      "Data": "{component-pod-view.cue文件对应内容}",
		      "Name": "component-pod-view.cue"
		    },
		    {
		      "Data": "{resource-view.cue文件对应内容}",
		      "Name": "resource-view.cue"
		    },
		    {
		      "Data": "{collect-logs.cue文件对应内容}",
		      "Name": "collect-logs.cue"
		    },
		    {
		      "Data": "{cloud-resource-view.cue文件对应内容}",
		      "Name": "cloud-resource-view.cue"
		    },
		    {
		      "Data": "{cloud-resource-secret-view.cue文件对应内容}",
		      "Name": "cloud-resource-secret-view.cue"
		    },
		    {
		      "Data": "{pod-view.cue文件对应内容}",
		      "Name": "pod-view.cue"
		    },
		    {
		      "Data": "{component-service-view.cue文件对应内容}",
		      "Name": "component-service-view.cue"
		    }
		  ],
		  "defSchemas": [
		    {
		      "Data": "{trait-uischema-storage.yaml文件对应内容}",
		      "Name": "trait-uischema-storage.yaml"
		    },
		    {
		      "Data": "{workflowstep-uischema-webhook.yaml文件对应内容}",
		      "Name": "workflowstep-uischema-webhook.yaml"
		    },
		    {
		      "Data": "{trait-uischema-scaler.yaml文件对应内容}",
		      "Name": "trait-uischema-scaler.yaml"
		    },
		    {
		      "Data": "{component-uischema-webservice.yaml文件对应内容}",
		      "Name": "component-uischema-webservice.yaml"
		    },
		    {
		      "Data": "{workflowstep-uischema-deploy2env.yaml文件对应内容}",
		      "Name": "workflowstep-uischema-deploy2env.yaml"
		    },
		    {
		      "Data": "{component-uischema-task.yaml文件对应内容}",
		      "Name": "component-uischema-task.yaml"
		    },
		    {
		      "Data": "{workflowstep-uischema-notification.yaml文件对应内容}",
		      "Name": "workflowstep-uischema-notification.yaml"
		    },
		    {
		      "Data": "{addon-uischema-velaux.yaml文件对应内容}",
		      "Name": "addon-uischema-velaux.yaml"
		    },
		    {
		      "Data": "{workflowstep-uischema-deploy.yaml文件对应内容}",
		      "Name": "workflowstep-uischema-deploy.yaml"
		    },
		    {
		      "Data": "{component-uischema-k8s-objects.yaml文件对应内容}",
		      "Name": "component-uischema-k8s-objects.yaml"
		    },
		    {
		      "Data": "{policy-uischema-override.yaml文件对应内容}",
		      "Name": "policy-uischema-override.yaml"
		    }
		  ],
		  "parameters": "parameter: {\n\t// +usage=Specify the image hub of velaux, eg. \"acr.kubevela.net\"\n\trepo?: string\n\t// +usage=Specify the database type, current support KubeAPI(default) and MongoDB.\n\tdbType: *\"kubeapi\" | \"mongodb\"\n\t// +usage=Specify the database name, for the kubeapi db type, it represents namespace.\n\tdatabase?: string\n\t// +usage=Specify the MongoDB URL. it only enabled where DB type is MongoDB.\n\tdbURL?: string\n\t// +usage=Specify the domain, if set, ingress will be created if the gateway driver is nginx.\n\tdomain?: string\n\t// +usage=Specify the name of the certificate cecret, if set, means enable the HTTPs.\n\tsecretName?: string\n\t// +usage=Specify the gateway type.\n\tgatewayDriver: *\"nginx\" | \"traefik\"\n\t// +usage=Specify the serviceAccountName for apiserver\n\tserviceAccountName: *\"kubevela-vela-core\" | string\n\t// +usage=Specify the service type.\n\tserviceType: *\"ClusterIP\" | \"NodePort\" | \"LoadBalancer\"\n\t// +usage=Specify the names of imagePullSecret for private image registry, eg. \"{a,b,c}\"\n\timagePullSecrets?: [...string]\n\t// +usage=Specify whether to enable the dex\n\tdex: *false | bool\n\t// +usage=Specify the replicas.\n\treplicas: *1 | int\n\t// +usage=Specify nodeport. This will be ignored if serviceType is not NodePort.\n\tnodePort: *30000 | int\n}\n",
		  "CUETemplates": [
		    {
		      "Data": "{apiserver.cue文件对应内容}",
		      "Name": "apiserver.cue"
		    },
		    {
		      "Data": "{velaux.cue文件对应内容}",
		      "Name": "velaux.cue"
		    }
		  ],
		  "appTemplate": null,
		  "appCueTemplate": {
		    "Data": "{template.cue文件对应内容}",
		    "Name": "template.cue"
		  }
		}

	*/
	for _, dep := range addon.Dependencies {
		_, err := FetchAddonRelatedApp(h.ctx, h.cli, dep.Name)
		if err == nil {
			continue
		}
		if !apierrors.IsNotFound(err) {
			return err
		}
		// always install addon's latest version
		depAddon, err := h.loadInstallPackage(dep.Name, "")
		if err != nil {
			return err
		}
		depHandler := *h
		depHandler.args = nil
		if err = depHandler.enableAddon(depAddon); err != nil {
			return errors.Wrap(err, "fail to dispatch dependent addon resource")
		}
	}
	return nil
}

// checkDependency checks if addon's dependency
func (h *Installer) checkDependency(addon *InstallPackage) ([]string, error) {
	var app v1beta1.Application
	var needEnable []string
	// 遍历 addon 的 Dependencies 字段
	// 在 ns=vela-system 中查找对应的 name=addon-${name} 的资源
	for _, dep := range addon.Dependencies {
		err := h.cli.Get(h.ctx, client.ObjectKey{
			Namespace: types.DefaultKubeVelaNS,
			Name:      addonutil.Addon2AppName(dep.Name),
		}, &app)
		if err == nil {
			continue
		}
		if !apierrors.IsNotFound(err) {
			return nil, err
		}
		// 到这一步一定是 addon-xx 没有找到, append 到 needEnable 中
		needEnable = append(needEnable, dep.Name)
	}
	return needEnable, nil
}
func (h *Installer) createOrUpdate(app *v1beta1.Application) error {
	var getapp v1beta1.Application
	err := h.cli.Get(h.ctx, client.ObjectKey{Name: app.Name, Namespace: app.Namespace}, &getapp)
	if apierrors.IsNotFound(err) {
		return h.cli.Create(h.ctx, app)
	}
	if err != nil {
		return err
	}
	getapp.Spec = app.Spec
	getapp.Labels = app.Labels
	getapp.Annotations = app.Annotations
	err = h.cli.Update(h.ctx, &getapp)
	if err != nil {
		klog.Errorf("fail to create application: %v", err)
		return errors.Wrap(err, "fail to create application")
	}
	getapp.DeepCopyInto(app)
	return nil
}

func (h *Installer) dispatchAddonResource(addon *InstallPackage) error {
	prettyPrint := func(d interface{}) {
		indent, _ := json.MarshalIndent(d, "", "  ")
		fmt.Println(string(indent))
	}
	// 渲染 addon 的cue模板
	app, auxiliaryOutputs, err := RenderApp(h.ctx, addon, h.cli, h.args)
	/*
		{
		  "kind": "Application",
		  "apiVersion": "core.oam.dev/v1beta1",
		  "metadata": {
		    "name": "addon-velaux",
		    "namespace": "vela-system",
		    "creationTimestamp": null,
		    "labels": {
		      "addons.oam.dev/name": "velaux",
		      "addons.oam.dev/version": "v1.5.8"
		    }
		  },
		  "spec": {
		    "components": [
		      {
		        "name": "apiserver",
		        "type": "webservice",
		        "properties": {
		          "cmd": [
		            "apiserver",
		            "--datastore-type=kubeapi"
		          ],
		          "image": "oamdev/vela-apiserver:v1.5.8",
		          "ports": [
		            {
		              "port": 8000,
		              "protocol": "TCP",
		              "expose": true
		            }
		          ]
		        },
		        "traits": [
		          {
		            "type": "service-account",
		            "properties": {
		              "name": "kubevela-vela-core"
		            }
		          },
		          {
		            "type": "scaler",
		            "properties": {
		              "replicas": 1
		            }
		          }
		        ]
		      },
		      {
		        "name": "velaux",
		        "type": "webservice",
		        "properties": {
		          "env": [
		            {
		              "name": "KUBEVELA_API_URL",
		              "value": "apiserver.vela-system:8000"
		            }
		          ],
		          "image": "oamdev/velaux:v1.5.8",
		          "ports": [
		            {
		              "port": 80,
		              "protocol": "TCP",
		              "expose": true
		            }
		          ],
		          "exposeType": "ClusterIP"
		        },
		        "dependsOn": [
		          "apiserver"
		        ],
		        "traits": [
		          {
		            "type": "scaler",
		            "properties": {
		              "replicas": 1
		            }
		          }
		        ]
		      }
		    ]
		  },
		  "status": {}
		}
	*/
	// 最终会渲染成 kubevela 的 Application 资源和一组k8s中[]*unstructured.Unstructured资源
	if err != nil {
		return errors.Wrap(err, "render addon application fail")
	}
	// 为addon的部署生成一个appname
	appName, err := determineAddonAppName(h.ctx, h.cli, h.addon.Name)
	if err != nil {
		return err
	}
	app.Name = appName // 对于velaux最终生成的name为addon-velaux
	// 配置label标签
	// 在原始资源的label上增加一个标签
	// 以 velaux 这个 addon为例, 会生成如下label标签
	// addons.oam.dev/registry: https://addons.kubevela.net
	app.SetLabels(util.MergeMapOverrideWithDst(app.GetLabels(), map[string]string{oam.LabelAddonRegistry: h.r.Name}))

	defs, err := RenderDefinitions(addon, h.config)
	if err != nil {
		return errors.Wrap(err, "render addon definitions fail")
	}

	if !h.overrideDefs {
		existDefs, err := checkConflictDefs(h.ctx, h.cli, defs, app.Name)
		if err != nil {
			return err
		}
		if len(existDefs) != 0 {
			return produceDefConflictError(existDefs)
		}
	}
	// prettyPrint(defs) []

	schemas, err := RenderDefinitionSchema(addon)
	if err != nil {
		return errors.Wrap(err, "render addon definitions' schema fail")
	}
	//prettyPrint(schemas)
	/*
		[
		  {
		    "apiVersion": "v1",
		    "data": {
		      "ui-schema": "[{\"jsonKey\":\"pvc\",\"label\":\"PersistentVolumeClaim\",\"sort\":1,\"subParameters\":[{\"jsonKey\":\"name\",\"sort\":1,\"validate\":{\"maxLength\":32,\"pattern\":\"^[a-z0-9]([-a-z0-9]*[a-z0-9])$\",\"required\":true}},{\"jsonKey\":\"mountPath\",\"sort\":3,\"validate\":{\"pattern\":\"^/(.*)$\",\"required\":true}},{\"jsonKey\":\"resources\",\"sort\":7,\"subParameters\":[{\"jsonKey\":\"requests\",\"sort\":1,\"style\":{\"colSpan\":12},\"subParameters\":[{\"jsonKey\":\"storage\",\"label\":\"Request Storage\",\"uiType\":\"DiskNumber\",\"validate\":{\"immutable\":true,\"required\":true}}],\"uiType\":\"Ignore\"},{\"jsonKey\":\"limits\",\"sort\":3,\"style\":{\"colSpan\":12},\"subParameters\":[{\"jsonKey\":\"storage\",\"label\":\"Limit Storage\",\"uiType\":\"DiskNumber\",\"validate\":{\"immutable\":true,\"required\":true}}],\"uiType\":\"Ignore\"}],\"validate\":{\"defaultValue\":{\"limits\":{\"storage\":\"8Gi\"},\"requests\":{\"storage\":\"8Gi\"}},\"required\":true}},{\"description\":\"If not specified, the cluster default StorageClass is used.\",\"jsonKey\":\"storageClassName\",\"sort\":9},{\"jsonKey\":\"accessModes\",\"sort\":11,\"validate\":{\"defaultValue\":[\"ReadWriteOnce\"],\"required\":false}},{\"description\":\"You can set the value of volumeMode to Block to use a volume as a raw block device.\",\"jsonKey\":\"volumeMode\",\"sort\":12,\"uiType\":\"Select\",\"validate\":{\"defaultValue\":\"Filesystem\",\"options\":[{\"label\":\"Filesystem\",\"value\":\"Filesystem\"},{\"label\":\"Block\",\"value\":\"Block\"}]}},{\"description\":\"The VolumeName is the binding reference to the PersistentVolume backing this claim.\",\"jsonKey\":\"volumeName\",\"sort\":14},{\"jsonKey\":\"selector\",\"sort\":17},{\"description\":\"It will create a new volume based on the contents of the specified data source.\",\"jsonKey\":\"dataSource\",\"sort\":19},{\"disable\":true,\"jsonKey\":\"dataSourceRef\",\"sort\":20},{\"disable\":true,\"jsonKey\":\"mountOnly\"}]},{\"disable\":true,\"jsonKey\":\"secret\",\"sort\":3},{\"disable\":true,\"jsonKey\":\"configMap\",\"sort\":5},{\"disable\":true,\"jsonKey\":\"emptyDir\"}]"
		    },
		    "kind": "ConfigMap",
		    "metadata": {
		      "creationTimestamp": null,
		      "name": "trait-uischema-storage",
		      "namespace": "vela-system"
		    }
		  },
		  {
		    "apiVersion": "v1",
		    "data": {
		      "ui-schema": "[{\"jsonKey\":\"url\",\"label\":\"URL\",\"sort\":1,\"subParameters\":[{\"jsonKey\":\"value\",\"label\":\"URL\",\"uiType\":\"Input\",\"validate\":{\"required\":true}}],\"uiType\":\"Ignore\"},{\"jsonKey\":\"data\",\"sort\":3}]"
		    },
		    "kind": "ConfigMap",
		    "metadata": {
		      "creationTimestamp": null,
		      "name": "workflowstep-uischema-webhook",
		      "namespace": "vela-system"
		    }
		  },
		  {
		    "apiVersion": "v1",
		    "data": {
		      "ui-schema": "[{\"jsonKey\":\"replicas\",\"validate\":{\"min\":0,\"required\":true}}]"
		    },
		    "kind": "ConfigMap",
		    "metadata": {
		      "creationTimestamp": null,
		      "name": "trait-uischema-scaler",
		      "namespace": "vela-system"
		    }
		  },
		  {
		    "apiVersion": "v1",
		    "data": {
		      "ui-schema": "[{\"jsonKey\":\"image\",\"label\":\"Container Image\",\"sort\":1,\"uiType\":\"ImageInput\"},{\"disable\":true,\"jsonKey\":\"imagePullSecret\",\"sort\":2},{\"description\":\"Specifies the memory resource required for the container, If set to 0, there is no limit.\",\"jsonKey\":\"memory\",\"label\":\"Memory\",\"sort\":4,\"style\":{\"colSpan\":12},\"uiType\":\"MemoryNumber\",\"validate\":{\"defaultValue\":\"1024Mi\",\"required\":true}},{\"description\":\"Specifies the cpu resource required for the container, If set to 0, there is no limit.\",\"jsonKey\":\"cpu\",\"label\":\"CPU\",\"sort\":5,\"style\":{\"colSpan\":12},\"uiType\":\"CPUNumber\",\"validate\":{\"defaultValue\":\"0.5\",\"required\":true}},{\"jsonKey\":\"exposeType\",\"sort\":6,\"validate\":null},{\"jsonKey\":\"ports\",\"label\":\"Service Ports\",\"sort\":7,\"subParameters\":[{\"jsonKey\":\"port\",\"sort\":1,\"validate\":{\"min\":1,\"required\":true}},{\"jsonKey\":\"protocol\",\"sort\":3},{\"disable\":true,\"jsonKey\":\"name\",\"sort\":4},{\"jsonKey\":\"expose\",\"sort\":5}],\"validate\":{\"defaultValue\":[{\"expose\":true,\"port\":80,\"protocol\":\"TCP\"}],\"required\":true}},{\"jsonKey\":\"cmd\",\"label\":\"CMD\",\"sort\":9},{\"jsonKey\":\"env\",\"label\":\"ENV\",\"sort\":10,\"subParameterGroupOption\":[{\"keys\":[\"name\",\"value\"],\"label\":\"Add By Value\"},{\"keys\":[\"name\",\"valueFrom\"],\"label\":\"Add By Secret\"}],\"subParameters\":[{\"jsonKey\":\"valueFrom\",\"label\":\"Secret Selector\",\"subParameters\":[{\"disable\":true,\"jsonKey\":\"configMapKeyRef\"},{\"jsonKey\":\"secretKeyRef\",\"subParameters\":[{\"jsonKey\":\"name\",\"label\":\"Secret Name\",\"sort\":1,\"uiType\":\"SecretSelect\"},{\"jsonKey\":\"key\",\"label\":\"Secret Key\",\"sort\":3,\"uiType\":\"SecretKeySelect\"}],\"uiType\":\"Ignore\"}],\"uiType\":\"Ignore\"}],\"uiType\":\"Structs\"},{\"description\":\"Set the path and type that the service needs to persist.\",\"jsonKey\":\"volumeMounts\",\"label\":\"Persistent Storage\",\"sort\":12,\"subParameters\":[{\"disable\":true,\"jsonKey\":\"configMap\"},{\"disable\":true,\"jsonKey\":\"secret\"},{\"jsonKey\":\"pvc\",\"label\":\"Storage By PVC\",\"sort\":1,\"subParameters\":[{\"jsonKey\":\"name\",\"sort\":1,\"validate\":{\"pattern\":\"^[a-z0-9]([-a-z0-9]*[a-z0-9])$\",\"required\":true}},{\"jsonKey\":\"claimName\",\"sort\":3},{\"jsonKey\":\"mountPath\",\"sort\":5,\"validate\":{\"pattern\":\"^/(.*)$\",\"required\":true}}]},{\"jsonKey\":\"hostPath\",\"label\":\"Storage By HostPath\",\"sort\":3,\"subParameters\":[{\"jsonKey\":\"name\",\"sort\":1,\"validate\":{\"pattern\":\"^[a-z0-9]([-a-z0-9]*[a-z0-9])$\",\"required\":true}},{\"jsonKey\":\"path\",\"label\":\"Host Path\",\"sort\":3,\"validate\":{\"pattern\":\"^/(.*)$\",\"required\":true}},{\"jsonKey\":\"mountPath\",\"sort\":5,\"validate\":{\"pattern\":\"^/(.*)$\",\"required\":true}}]},{\"jsonKey\":\"emptyDir\",\"label\":\"Temporary Storage\",\"sort\":5,\"subParameters\":[{\"jsonKey\":\"name\",\"sort\":1,\"validate\":{\"pattern\":\"^[a-z0-9]([-a-z0-9]*[a-z0-9])$\",\"required\":true}},{\"jsonKey\":\"medium\",\"sort\":3,\"validate\":{\"options\":[{\"label\":\"Memory\",\"value\":\"memory\"},{\"label\":\"Dir\",\"value\":\"\"}],\"required\":false}},{\"jsonKey\":\"mountPath\",\"sort\":5,\"validate\":{\"pattern\":\"^/(.*)$\",\"required\":true}}]}],\"uiType\":\"Group\"},{\"jsonKey\":\"readinessProbe\",\"label\":\"ReadinessProbe\",\"sort\":13,\"subParameters\":[{\"disable\":true,\"jsonKey\":\"hostAliases\"},{\"jsonKey\":\"timeoutSeconds\",\"sort\":1,\"style\":{\"colSpan\":12}},{\"jsonKey\":\"failureThreshold\",\"sort\":4,\"style\":{\"colSpan\":12}},{\"jsonKey\":\"initialDelaySeconds\",\"sort\":7,\"style\":{\"colSpan\":12},\"validate\":{\"defaultValue\":5,\"required\":true}},{\"jsonKey\":\"periodSeconds\",\"sort\":9,\"style\":{\"colSpan\":12}},{\"jsonKey\":\"successThreshold\",\"sort\":11,\"style\":{\"colSpan\":12}},{\"jsonKey\":\"exec\",\"sort\":14},{\"jsonKey\":\"httpGet\",\"sort\":19,\"subParameters\":[{\"jsonKey\":\"port\",\"sort\":1,\"style\":{\"colSpan\":12},\"validate\":{\"min\":1,\"required\":true}},{\"jsonKey\":\"path\",\"sort\":3,\"style\":{\"colSpan\":12},\"validate\":{\"pattern\":\"^/(.*)$\",\"required\":true}},{\"jsonKey\":\"httpHeaders\",\"sort\":5}]},{\"jsonKey\":\"tcpSocket\",\"sort\":19,\"subParameters\":[{\"jsonKey\":\"port\",\"validate\":{\"min\":1,\"required\":true}}]}],\"uiType\":\"Group\"},{\"jsonKey\":\"livenessProbe\",\"label\":\"LivenessProbe\",\"sort\":15,\"subParameters\":[{\"disable\":true,\"jsonKey\":\"hostAliases\"},{\"jsonKey\":\"timeoutSeconds\",\"sort\":1,\"style\":{\"colSpan\":12}},{\"jsonKey\":\"failureThreshold\",\"sort\":4,\"style\":{\"colSpan\":12}},{\"jsonKey\":\"initialDelaySeconds\",\"sort\":7,\"style\":{\"colSpan\":12},\"validate\":{\"defaultValue\":5,\"required\":true}},{\"jsonKey\":\"periodSeconds\",\"sort\":9,\"style\":{\"colSpan\":12}},{\"jsonKey\":\"successThreshold\",\"sort\":11,\"style\":{\"colSpan\":12}},{\"jsonKey\":\"exec\",\"sort\":14},{\"jsonKey\":\"httpGet\",\"sort\":19,\"subParameters\":[{\"jsonKey\":\"port\",\"sort\":1,\"style\":{\"colSpan\":12},\"validate\":{\"min\":1,\"required\":true}},{\"jsonKey\":\"path\",\"sort\":3,\"style\":{\"colSpan\":12},\"validate\":{\"pattern\":\"^/(.*)$\",\"required\":true}},{\"jsonKey\":\"httpHeaders\",\"sort\":5}]},{\"jsonKey\":\"tcpSocket\",\"sort\":19,\"subParameters\":[{\"jsonKey\":\"port\",\"validate\":{\"min\":1,\"required\":true}}]}],\"uiType\":\"Group\"},{\"jsonKey\":\"annotations\",\"sort\":19},{\"jsonKey\":\"labels\",\"sort\":21},{\"description\":\"Specify image pull policy for your service\",\"jsonKey\":\"imagePullPolicy\",\"label\":\"Image Pull Policy\",\"sort\":24,\"uiType\":\"Select\",\"validate\":{\"defaultValue\":\"IfNotPresent\",\"options\":[{\"label\":\"IfNotPresent\",\"value\":\"IfNotPresent\"},{\"label\":\"Always\",\"value\":\"Always\"},{\"label\":\"Never\",\"value\":\"Never\"}]}},{\"disable\":true,\"jsonKey\":\"addRevisionLabel\"},{\"disable\":true,\"jsonKey\":\"port\"},{\"disable\":true,\"jsonKey\":\"volumes\"}]"
		    },
		    "kind": "ConfigMap",
		    "metadata": {
		      "creationTimestamp": null,
		      "name": "component-uischema-webservice",
		      "namespace": "vela-system"
		    }
		  },
		  {
		    "apiVersion": "v1",
		    "data": {
		      "ui-schema": "[{\"jsonKey\":\"parallel\",\"sort\":5,\"validate\":{\"defaultValue\":false}},{\"jsonKey\":\"policy\",\"sort\":1},{\"jsonKey\":\"env\",\"sort\":3}]"
		    },
		    "kind": "ConfigMap",
		    "metadata": {
		      "creationTimestamp": null,
		      "name": "workflowstep-uischema-deploy2env",
		      "namespace": "vela-system"
		    }
		  },
		  {
		    "apiVersion": "v1",
		    "data": {
		      "ui-schema": "[{\"jsonKey\":\"image\",\"label\":\"Image\",\"sort\":1,\"uiType\":\"ImageInput\"},{\"disable\":true,\"jsonKey\":\"imagePullSecret\",\"sort\":2},{\"jsonKey\":\"restart\",\"sort\":3,\"uiType\":\"Select\",\"validate\":{\"defaultValue\":\"Never\",\"options\":[{\"label\":\"Never\",\"value\":\"Never\"},{\"label\":\"OnFailure\",\"value\":\"OnFailure\"}],\"required\":true}},{\"jsonKey\":\"count\",\"sort\":4,\"uiType\":\"Number\",\"validate\":{\"defaultValue\":1,\"max\":128,\"min\":0,\"required\":true}},{\"description\":\"Specifies the memory resource required for the container, If set to 0, there is no limit.\",\"jsonKey\":\"memory\",\"label\":\"Memory\",\"sort\":5,\"style\":{\"colSpan\":12},\"uiType\":\"MemoryNumber\",\"validate\":{\"defaultValue\":\"1024Mi\",\"required\":true}},{\"description\":\"Specifies the cpu resource required for the container, If set to 0, there is no limit.\",\"jsonKey\":\"cpu\",\"label\":\"CPU\",\"sort\":7,\"style\":{\"colSpan\":12},\"uiType\":\"CPUNumber\",\"validate\":{\"defaultValue\":\"0.5\",\"required\":true}},{\"jsonKey\":\"cmd\",\"label\":\"CMD\",\"sort\":9},{\"jsonKey\":\"env\",\"label\":\"ENV\",\"sort\":10,\"subParameterGroupOption\":[{\"keys\":[\"name\",\"value\"],\"label\":\"Add By Value\"},{\"keys\":[\"name\",\"valueFrom\"],\"label\":\"Add By Secret\"}],\"subParameters\":[{\"jsonKey\":\"valueFrom\",\"label\":\"Secret Selector\",\"subParameters\":[{\"disable\":true,\"jsonKey\":\"configMapKeyRef\"},{\"jsonKey\":\"secretKeyRef\",\"subParameters\":[{\"jsonKey\":\"name\",\"label\":\"Secret Name\",\"sort\":1,\"uiType\":\"SecretSelect\"},{\"jsonKey\":\"key\",\"label\":\"Secret Key\",\"sort\":3,\"uiType\":\"SecretKeySelect\"}],\"uiType\":\"Ignore\"}],\"uiType\":\"Ignore\"}],\"uiType\":\"Structs\"},{\"jsonKey\":\"readinessProbe\",\"label\":\"ReadinessProbe\",\"sort\":13,\"subParameters\":[{\"disable\":true,\"jsonKey\":\"hostAliases\"},{\"jsonKey\":\"timeoutSeconds\",\"sort\":1,\"style\":{\"colSpan\":12}},{\"jsonKey\":\"failureThreshold\",\"sort\":4,\"style\":{\"colSpan\":12}},{\"jsonKey\":\"initialDelaySeconds\",\"sort\":7,\"style\":{\"colSpan\":12},\"validate\":{\"defaultValue\":5,\"required\":true}},{\"jsonKey\":\"periodSeconds\",\"sort\":9,\"style\":{\"colSpan\":12}},{\"jsonKey\":\"successThreshold\",\"sort\":11,\"style\":{\"colSpan\":12}},{\"jsonKey\":\"exec\",\"sort\":14},{\"jsonKey\":\"httpGet\",\"sort\":19,\"subParameters\":[{\"jsonKey\":\"port\",\"sort\":1,\"style\":{\"colSpan\":12},\"validate\":{\"min\":1,\"required\":true}},{\"jsonKey\":\"path\",\"sort\":3,\"style\":{\"colSpan\":12},\"validate\":{\"pattern\":\"^/(.*)$\",\"required\":true}},{\"jsonKey\":\"httpHeaders\",\"sort\":5}]},{\"jsonKey\":\"tcpSocket\",\"sort\":19,\"subParameters\":[{\"jsonKey\":\"port\",\"validate\":{\"min\":1,\"required\":true}}]}],\"uiType\":\"Group\"},{\"jsonKey\":\"livenessProbe\",\"label\":\"LivenessProbe\",\"sort\":15,\"subParameters\":[{\"disable\":true,\"jsonKey\":\"hostAliases\"},{\"jsonKey\":\"timeoutSeconds\",\"sort\":1,\"style\":{\"colSpan\":12}},{\"jsonKey\":\"failureThreshold\",\"sort\":4,\"style\":{\"colSpan\":12}},{\"jsonKey\":\"initialDelaySeconds\",\"sort\":7,\"style\":{\"colSpan\":12},\"validate\":{\"defaultValue\":5,\"required\":true}},{\"jsonKey\":\"periodSeconds\",\"sort\":9,\"style\":{\"colSpan\":12}},{\"jsonKey\":\"successThreshold\",\"sort\":11,\"style\":{\"colSpan\":12}},{\"jsonKey\":\"exec\",\"sort\":14},{\"jsonKey\":\"httpGet\",\"sort\":19,\"subParameters\":[{\"jsonKey\":\"port\",\"sort\":1,\"style\":{\"colSpan\":12},\"validate\":{\"min\":1,\"required\":true}},{\"jsonKey\":\"path\",\"sort\":3,\"style\":{\"colSpan\":12},\"validate\":{\"pattern\":\"^/(.*)$\",\"required\":true}},{\"jsonKey\":\"httpHeaders\",\"sort\":5}]},{\"jsonKey\":\"tcpSocket\",\"sort\":19,\"subParameters\":[{\"jsonKey\":\"port\",\"validate\":{\"min\":1,\"required\":true}}]}],\"uiType\":\"Group\"},{\"jsonKey\":\"annotations\",\"sort\":19},{\"jsonKey\":\"labels\",\"sort\":21},{\"description\":\"Specify image pull policy for your service\",\"jsonKey\":\"imagePullPolicy\",\"label\":\"Image Pull Policy\",\"sort\":24,\"uiType\":\"Select\",\"validate\":{\"defaultValue\":\"IfNotPresent\",\"options\":[{\"label\":\"IfNotPresent\",\"value\":\"IfNotPresent\"},{\"label\":\"Always\",\"value\":\"Always\"},{\"label\":\"Never\",\"value\":\"Never\"}]}},{\"disable\":true,\"jsonKey\":\"volumes\"}]"
		    },
		    "kind": "ConfigMap",
		    "metadata": {
		      "creationTimestamp": null,
		      "name": "component-uischema-task",
		      "namespace": "vela-system"
		    }
		  },
		  {
		    "apiVersion": "v1",
		    "data": {
		      "ui-schema": "[{\"description\":\"Support push messages to Dingding Group.\",\"jsonKey\":\"dingding\",\"sort\":1,\"subParameters\":[{\"jsonKey\":\"url\",\"sort\":1,\"subParameters\":[{\"jsonKey\":\"value\",\"label\":\"Webhook\",\"uiType\":\"Input\",\"validate\":{\"required\":true}}],\"uiType\":\"Ignore\"},{\"description\":\"Specify the message that you want to sent\",\"jsonKey\":\"message\",\"sort\":3,\"subParameters\":[{\"disable\":true,\"jsonKey\":\"msgtype\"},{\"disable\":true,\"jsonKey\":\"link\"},{\"disable\":true,\"jsonKey\":\"markdown\"},{\"disable\":true,\"jsonKey\":\"at\"},{\"disable\":true,\"jsonKey\":\"actionCard\"},{\"disable\":true,\"jsonKey\":\"feedCard\"},{\"jsonKey\":\"text\",\"subParameters\":[{\"description\":\"Specify the message that you want to sent\",\"jsonKey\":\"content\",\"label\":\"Message\"}],\"uiType\":\"Ignore\"}],\"uiType\":\"Ignore\"}]},{\"description\":\"Support push email message.\",\"jsonKey\":\"email\",\"sort\":3,\"subParameters\":[{\"jsonKey\":\"from\",\"sort\":1,\"subParameters\":[{\"jsonKey\":\"host\",\"sort\":1},{\"jsonKey\":\"port\",\"sort\":3},{\"jsonKey\":\"address\",\"sort\":5},{\"jsonKey\":\"password\",\"sort\":7,\"subParameters\":[{\"jsonKey\":\"value\",\"label\":\"Password\",\"uiType\":\"Password\",\"validate\":{\"required\":true}}],\"uiType\":\"Ignore\"},{\"jsonKey\":\"alias\",\"sort\":9}]},{\"jsonKey\":\"to\",\"sort\":3},{\"jsonKey\":\"content\",\"sort\":5,\"subParameters\":[{\"jsonKey\":\"subject\",\"sort\":1},{\"jsonKey\":\"body\",\"sort\":3}]}]},{\"description\":\"Support push messages to slack channel.\",\"jsonKey\":\"slack\",\"sort\":5,\"subParameters\":[{\"jsonKey\":\"url\",\"sort\":1,\"subParameters\":[{\"jsonKey\":\"value\",\"label\":\"Webhook\",\"uiType\":\"Input\",\"validate\":{\"required\":true}}],\"uiType\":\"Ignore\"},{\"jsonKey\":\"message\",\"subParameters\":[{\"disable\":true,\"jsonKey\":\"blocks\"},{\"disable\":true,\"jsonKey\":\"attachments\"},{\"disable\":true,\"jsonKey\":\"thread_ts\"},{\"disable\":true,\"jsonKey\":\"mrkdwn\"},{\"description\":\"Specify the message that you want to sent\",\"jsonKey\":\"text\",\"label\":\"Message\"}],\"uiType\":\"Ignore\"}]}]"
		    },
		    "kind": "ConfigMap",
		    "metadata": {
		      "creationTimestamp": null,
		      "name": "workflowstep-uischema-notification",
		      "namespace": "vela-system"
		    }
		  },
		  {
		    "apiVersion": "v1",
		    "data": {
		      "ui-schema": "[{\"jsonKey\":\"dbType\",\"label\":\"DBType\",\"sort\":1,\"validate\":{\"defaultValue\":\"kubeapi\",\"options\":[{\"label\":\"MongoDB\",\"value\":\"mongodb\"},{\"label\":\"KubeAPI\",\"value\":\"kubeapi\"}],\"required\":true}},{\"conditions\":[{\"jsonKey\":\"dbType\",\"op\":\"==\",\"value\":\"mongodb\"}],\"jsonKey\":\"dbURL\",\"label\":\"DatabaseURL\",\"sort\":3,\"validate\":{\"required\":true}},{\"jsonKey\":\"database\",\"sort\":7,\"validate\":{\"defaultValue\":\"kubevela\",\"required\":true}},{\"disable\":true,\"jsonKey\":\"serviceAccountName\"},{\"jsonKey\":\"serviceType\",\"sort\":8},{\"jsonKey\":\"domain\",\"sort\":9},{\"jsonKey\":\"gatewayDriver\",\"sort\":10},{\"jsonKey\":\"repo\",\"sort\":11},{\"jsonKey\":\"imagePullSecrets\",\"sort\":13}]"
		    },
		    "kind": "ConfigMap",
		    "metadata": {
		      "creationTimestamp": null,
		      "name": "addon-uischema-velaux",
		      "namespace": "vela-system"
		    }
		  },
		  {
		    "apiVersion": "v1",
		    "data": {
		      "ui-schema": "[{\"jsonKey\":\"policies\",\"label\":\"Policies\",\"sort\":10,\"uiType\":\"PolicySelect\",\"validate\":{\"required\":true}},{\"jsonKey\":\"parallelism\",\"sort\":20,\"style\":{\"colSpan\":12}},{\"jsonKey\":\"auto\",\"sort\":30,\"style\":{\"colSpan\":12}}]"
		    },
		    "kind": "ConfigMap",
		    "metadata": {
		      "creationTimestamp": null,
		      "name": "workflowstep-uischema-deploy",
		      "namespace": "vela-system"
		    }
		  },
		  {
		    "apiVersion": "v1",
		    "data": {
		      "ui-schema": "[{\"jsonKey\":\"objects\",\"uiType\":\"K8sObjectsCode\"}]"
		    },
		    "kind": "ConfigMap",
		    "metadata": {
		      "creationTimestamp": null,
		      "name": "component-uischema-k8s-objects",
		      "namespace": "vela-system"
		    }
		  },
		  {
		    "apiVersion": "v1",
		    "data": {
		      "ui-schema": "[{\"jsonKey\":\"selector\",\"sort\":100,\"uiType\":\"ComponentSelect\"},{\"jsonKey\":\"components\",\"sort\":101,\"uiType\":\"ComponentPatches\"}]"
		    },
		    "kind": "ConfigMap",
		    "metadata": {
		      "creationTimestamp": null,
		      "name": "policy-uischema-override",
		      "namespace": "vela-system"
		    }
		  }
		]
	*/

	views, err := RenderViews(addon)
	if err != nil {
		return errors.Wrap(err, "render addon views fail")
	}
	prettyPrint(views)

	if err := passDefInAppAnnotation(defs, app); err != nil {
		return errors.Wrapf(err, "cannot pass definition to addon app's annotation")
	}

	// !!! 拉起对应的 app 资源
	// 拉起之后后面会有对应的Applicaion对应的Operator来处理，核心在于Reconcile函数
	if err = h.createOrUpdate(app); err != nil {
		return err
	}

	// 这里几个资源 apply 方式都是走的 unstructured.Unstructured 方式
	// unstructured.Unstructured 可以理解就是写个 k8s 的yaml文件，然后执行apply -f 的操作,
	// 只不过这里采用了内存对象，而不是文件
	// 需要补充下集中类型对应的资源yaml案例，不然很难追踪到对应的crd资源
	for _, def := range defs {
		if !checkBondComponentExist(*def, *app) {
			continue
		}
		// if binding component exist, apply the definition
		addOwner(def, app)
		// createOrGetExisting:pkg/utils/apply/apply.go:208
		err = h.apply.Apply(h.ctx, def, apply.DisableUpdateAnnotation())
		if err != nil {
			return err
		}
	}

	for _, schema := range schemas {
		addOwner(schema, app)
		err = h.apply.Apply(h.ctx, schema, apply.DisableUpdateAnnotation())
		if err != nil {
			return err
		}
	}

	for _, view := range views {
		addOwner(view, app)
		err = h.apply.Apply(h.ctx, view, apply.DisableUpdateAnnotation())
		if err != nil {
			return err
		}
	}

	for _, o := range auxiliaryOutputs {
		if !checkBondComponentExist(*o, *app) {
			continue
		}
		addOwner(o, app)
		err = h.apply.Apply(h.ctx, o, apply.DisableUpdateAnnotation())
		if err != nil {
			return err
		}
	}

	if h.args != nil && len(h.args) > 0 {
		sec := RenderArgsSecret(addon, h.args)
		addOwner(sec, app)
		//prettyPrint(sec)
		err = h.apply.Apply(h.ctx, sec, apply.DisableUpdateAnnotation())
		if err != nil {
			return err
		}
	}
	return nil
}

// this func will handle such two case
// 1. if last apply failed an workflow have suspend, this func will continue the workflow
// 2. restart the workflow, if the new cluster have been added in KubeVela
func (h *Installer) continueOrRestartWorkflow() error {
	app, err := FetchAddonRelatedApp(h.ctx, h.cli, h.addon.Name)
	if err != nil {
		return err
	}

	switch {
	// this case means user add a new cluster and user want to restart workflow to dispatch addon resources to new cluster
	// re-apply app won't help app restart workflow
	case app.Status.Phase == common2.ApplicationRunning:
		// we can use retry on conflict here in CLI, because we want to update the status in this CLI operation.
		return retry.RetryOnConflict(retry.DefaultBackoff, func() (err error) {
			if err = h.cli.Get(h.ctx, client.ObjectKey{Namespace: app.Namespace, Name: app.Name}, app); err != nil {
				return
			}
			app.Status.Workflow = nil
			return h.cli.Status().Update(h.ctx, app)
		})
	// this case means addon last installation meet some error and workflow has been suspended by app controller
	// re-apply app won't help app workflow continue
	case app.Status.Workflow != nil && app.Status.Workflow.Suspend:
		// we can use retry on conflict here in CLI, because we want to update the status in this CLI operation.
		return retry.RetryOnConflict(retry.DefaultBackoff, func() (err error) {
			if err = h.cli.Get(h.ctx, client.ObjectKey{Namespace: app.Namespace, Name: app.Name}, app); err != nil {
				return
			}
			mergePatch := client.MergeFrom(app.DeepCopy())
			app.Status.Workflow.Suspend = false
			return h.cli.Status().Patch(h.ctx, app, mergePatch)
		})
	}
	return nil
}

// getAddonVersionMeetSystemRequirement return the addon's latest version which meet the system requirements
func (h *Installer) getAddonVersionMeetSystemRequirement(addonName string) string {
	if h.r != nil && IsVersionRegistry(*h.r) {
		// 从远程的registry中获取符合系统要求的最新的版本的addon
		versionedRegistry := BuildVersionedRegistry(h.r.Name, h.r.Helm.URL, &common.HTTPOption{
			Username: h.r.Helm.Username,
			Password: h.r.Helm.Password,
		})
		versions, err := versionedRegistry.GetAddonAvailableVersion(addonName)
		if err != nil {
			return ""
		}
		for _, version := range versions {
			req := LoadSystemRequirements(version.Annotations)
			if checkAddonVersionMeetRequired(h.ctx, req, h.cli, h.dc) == nil {
				return version.Version
			}
		}
	}
	return ""
}

func addOwner(child *unstructured.Unstructured, app *v1beta1.Application) {
	child.SetOwnerReferences(append(child.GetOwnerReferences(),
		*metav1.NewControllerRef(app, v1beta1.ApplicationKindVersionKind)))
}

// determine app name, if app is already exist, use the application name
func determineAddonAppName(ctx context.Context, cli client.Client, addonName string) (string, error) {
	// 根据 ns=vela-system & name=addon-${addonName}获取applicaiton的资源信息
	app, err := FetchAddonRelatedApp(ctx, cli, addonName)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return "", err
		}
		// if the app still not exist, use addon-{addonName}
		// 不存在按照addon-{addonName}的方式命名
		return addonutil.Addon2AppName(addonName), nil
	}
	// 已经存在直接复用之前生成的name
	return app.Name, nil
}

// FetchAddonRelatedApp will fetch the addon related app, this func will use NamespacedName(vela-system, addon-addonName) to get app
// if not find will try to get 1.1 legacy addon related app by using NamespacedName(vela-system, `addonName`)
// 获取addon关联的app
// 1.1 之前的版本 name={addonName} 		 namespace=vela-system
// 1.1 之后的版本 name=addon-{addonName}   namespace=vela-system
func FetchAddonRelatedApp(ctx context.Context, cli client.Client, addonName string) (*v1beta1.Application, error) {
	app := &v1beta1.Application{}
	if err := cli.Get(ctx, types2.NamespacedName{Namespace: types.DefaultKubeVelaNS, Name: addonutil.Addon2AppName(addonName)}, app); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, err
		}
		// for 1.1 addon app compatibility code
		if err := cli.Get(ctx, types2.NamespacedName{Namespace: types.DefaultKubeVelaNS, Name: addonName}, app); err != nil {
			return nil, err
		}
	}
	return app, nil
}

// checkAddonVersionMeetRequired will check the version of cli/ux and kubevela-core-controller whether meet the addon requirement, if not will return an error
// please notice that this func is for check production environment which vela cli/ux or vela core is officalVersion
// if version is for test or debug eg: latest/commit-id/branch-name this func will return nil error
func checkAddonVersionMeetRequired(ctx context.Context, require *SystemRequirements, k8sClient client.Client, dc *discovery.DiscoveryClient) error {
	if require == nil {
		return nil
	}

	// if not semver version, bypass check cli/ux. eg: {branch name/git commit id/UNKNOWN}
	// 检查 vela-cli 的版本是否符合 addon 的要求
	if version2.IsOfficialKubeVelaVersion(version2.VelaVersion) {
		res, err := checkSemVer(version2.VelaVersion, require.VelaVersion)
		if err != nil {
			return err
		}
		if !res {
			return fmt.Errorf("vela cli/ux version: %s  require: %s", version2.VelaVersion, require.VelaVersion)
		}
	}

	// check vela core controller version
	// 检查集群中的vela-core的版本 imageVersion => v1.5.7
	imageVersion, err := fetchVelaCoreImageTag(ctx, k8sClient)
	if err != nil {
		return err
	}

	// if not semver version, bypass check vela-core.
	if version2.IsOfficialKubeVelaVersion(imageVersion) { // imageVersion:v1.5.7 require.VelaVersion:>=v1.5.0
		res, err := checkSemVer(imageVersion, require.VelaVersion)
		if err != nil {
			return err
		}
		if !res {
			return fmt.Errorf("the vela core controller: %s require: %s", imageVersion, require.VelaVersion)
		}
	}

	// discovery client is nil so bypass check kubernetes version
	if dc == nil {
		return nil
	}
	// 检查 k8s 的版本是否符合 addon 的要求
	k8sVersion, err := dc.ServerVersion()
	if err != nil {
		return err
	}
	// if not semver version, bypass check kubernetes version.
	/*
		version.Info{
			Major:"1",
			Minor:"19",
			GitVersion:"v1.19.16",
			GitCommit:"e37e4ab4cc8dcda84f1344dda47a97bb1927d074",
			GitTreeState:"clean",
			BuildDate:"2021-10-27T16:20:18Z",
			GoVersion:"go1.15.15",
			Compiler:"gc",
			Platform:"linux/amd64",
		}
	*/
	if version2.IsOfficialKubeVelaVersion(k8sVersion.GitVersion) {
		res, err := checkSemVer(k8sVersion.GitVersion, require.KubernetesVersion)
		if err != nil {
			return err
		}

		if !res {
			return fmt.Errorf("the kubernetes version %s require: %s", k8sVersion.GitVersion, require.KubernetesVersion)
		}
	}

	return nil
}

func checkSemVer(actual string, require string) (bool, error) {
	if len(require) == 0 {
		return true, nil
	}
	semVer := strings.TrimPrefix(actual, "v")
	// 这里的 require 可能形式如 ">=v1.5.0" 或者 ">=v1.2.0, <v1.5.0", 所以用 ReplaceAll
	l := strings.ReplaceAll(require, "v", " ")
	constraint, err := semver.NewConstraint(l)
	if err != nil {
		log.Logger.Errorf("fail to new constraint: %s", err.Error())
		return false, err
	}
	v, err := semver.NewVersion(semVer)
	if err != nil {
		log.Logger.Errorf("fail to new version %s: %s", semVer, err.Error())
		return false, err
	}
	if constraint.Check(v) {
		return true, nil
	}
	if strings.Contains(actual, "-") && !strings.Contains(require, "-") {
		semVer := strings.TrimPrefix(actual[:strings.Index(actual, "-")], "v")
		if strings.Contains(require, ">=") && require[strings.Index(require, "=")+1:] == semVer {
			// for case: `actual` is 1.5.0-beta.1 require is >=`1.5.0`
			return false, nil
		}
		v, err := semver.NewVersion(semVer)
		if err != nil {
			log.Logger.Errorf("fail to new version %s: %s", semVer, err.Error())
			return false, err
		}
		if constraint.Check(v) {
			return true, nil
		}
	}
	return false, nil
}

func fetchVelaCoreImageTag(ctx context.Context, k8sClient client.Client) (string, error) {
	// 获取 vela-core 对应的 deployment 中的镜像对应的版本号
	deployList := &appsv1.DeploymentList{}
	// 先尝试根据 -l "controller.oam.dev/name"="vela-core" 获取 deployment
	if err := k8sClient.List(ctx, deployList, client.MatchingLabels{oam.LabelControllerName: oam.ApplicationControllerName}); err != nil {
		return "", err
	}
	deploy := appsv1.Deployment{}
	if len(deployList.Items) == 0 {
		// backward compatible logic old version which vela-core controller has no this label
		// 兼容老版本的根据 ns=vela-system name=kubevela-vela-core 获取 deployment
		if err := k8sClient.Get(ctx, types2.NamespacedName{Namespace: types.DefaultKubeVelaNS, Name: types.KubeVelaControllerDeployment}, &deploy); err != nil {
			if apierrors.IsNotFound(err) {
				return "", errors.New("can't find a running KubeVela instance, please install it first")
			}
			return "", err
		}
	} else {
		deploy = deployList.Items[0]
	}

	var tag string
	for _, c := range deploy.Spec.Template.Spec.Containers {
		// 遍历 deployment 中所有的 contianser，找到 name=kubevela 的 container
		if c.Name == types.DefaultKubeVelaReleaseName {
			l := strings.Split(c.Image, ":")
			if len(l) == 1 { // 没有tag, docker默认策略为 latest
				// if tag is empty mean use latest image
				return "latest", nil
			} // 否则为split出来的index=1的item
			tag = l[1]
		}
	}
	return tag, nil
}

// PackageAddon package vela addon directory into a helm chart compatible archive and return its absolute path
func PackageAddon(addonDictPath string) (string, error) {
	// save the Chart.yaml file in order to be compatible with helm chart
	err := MakeChartCompatible(addonDictPath, true)
	if err != nil {
		return "", err
	}

	ch, err := loader.LoadDir(addonDictPath)
	if err != nil {
		return "", err
	}

	dest, err := os.Getwd()
	if err != nil {
		return "", err
	}
	archive, err := chartutil.Save(ch, dest)
	if err != nil {
		return "", err
	}
	return archive, nil
}

// GetAddonLegacyParameters get addon's legacy parameters, that is stored in Secret
func GetAddonLegacyParameters(ctx context.Context, k8sClient client.Client, addonName string) (map[string]interface{}, error) {
	var sec v1.Secret
	err := k8sClient.Get(ctx, client.ObjectKey{Namespace: types.DefaultKubeVelaNS, Name: addonutil.Addon2SecName(addonName)}, &sec)
	if err != nil {
		return nil, err
	}
	args, err := FetchArgsFromSecret(&sec)
	if err != nil {
		return nil, err
	}
	return args, nil
}

// MergeAddonInstallArgs merge addon's legacy parameter and new input args
func MergeAddonInstallArgs(ctx context.Context, k8sClient client.Client, addonName string, args map[string]interface{}) (map[string]interface{}, error) {
	legacyParams, err := GetAddonLegacyParameters(ctx, k8sClient, addonName)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, err
		}
		return args, nil
	}

	if args == nil && legacyParams == nil {
		return args, nil
	}

	r := make(map[string]interface{})
	if err := mergo.Merge(&r, legacyParams, mergo.WithOverride); err != nil {
		return nil, err
	}

	if err := mergo.Merge(&r, args, mergo.WithOverride); err != nil {
		return nil, err
	}
	return r, nil
}

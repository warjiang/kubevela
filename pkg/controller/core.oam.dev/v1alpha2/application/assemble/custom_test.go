package assemble

import (
	"github.com/oam-dev/cluster-gateway/pkg/generated/clientset/versioned/scheme"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1alpha1"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1alpha2"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/appfile"
	"github.com/oam-dev/kubevela/pkg/cue/packages"
	"github.com/oam-dev/kubevela/pkg/oam/discoverymapper"
	"github.com/stretchr/testify/suite"
	corev1 "k8s.io/api/core/v1"
	crdv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/pointer"
	"math/rand"
	"os"
	"path/filepath"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
	"testing"
	"time"
)

func TestAssembleSuite(t *testing.T) {
	suite.Run(t, new(AssembleTestSuite))
}

type AssembleTestSuite struct {
	suite.Suite
}

func (s *AssembleTestSuite) SetupTest() {
	s.T().Log("steup test")
	rand.Seed(time.Now().UnixNano())
	var yamlPath string
	if _, set := os.LookupEnv("COMPATIBILITY_TEST"); set {
		yamlPath = "../../../../../test/compatibility-test/testdata"
	} else {
		yamlPath = filepath.Join("../../../../..", "charts", "vela-core", "crds")
	}
	testEnv = &envtest.Environment{
		ControlPlaneStartTimeout: time.Minute,
		ControlPlaneStopTimeout:  time.Minute,
		UseExistingCluster:       pointer.BoolPtr(false),
		CRDDirectoryPaths:        []string{yamlPath},
	}

	var err error
	cfg, err = testEnv.Start()
	err = v1alpha2.SchemeBuilder.AddToScheme(testScheme)
	err = v1alpha1.SchemeBuilder.AddToScheme(testScheme)
	err = v1beta1.SchemeBuilder.AddToScheme(testScheme)
	err = scheme.AddToScheme(testScheme)
	err = crdv1.AddToScheme(testScheme)
	s.Assert().Nil(err, "should not have error when adding scheme")

	// +kubebuilder:scaffold:scheme
	k8sClient, err = client.New(cfg, client.Options{Scheme: testScheme})
	dm, err := discoverymapper.New(cfg)
	pd, err := packages.NewPackageDiscover(cfg)
	appParser = appfile.NewApplicationParser(k8sClient, dm, pd)
}

func (s *AssembleTestSuite) TearDownTest() {
	s.T().Log("teardown test")
	err := testEnv.Stop()
	s.Assert().Nil(err, "should not have error when tear down")
}

func (s *AssembleTestSuite) TestAssemble() {
	var (
		compName  = "test-comp"
		namespace = "default"
	)
	// 读文件构造 v1beta1.ApplicationRevision
	appRev := &v1beta1.ApplicationRevision{}
	b, err := os.ReadFile("./testdata/apprevision.yaml")
	err = yaml.Unmarshal(b, appRev)
	s.Assert().Nil(err, "should not have error when unmarshal appRevision")

	ao := NewAppManifests(appRev, appParser)
	workloads, traits, _, err := ao.GroupAssembledManifests()

	allResources, err := ao.AssembledManifests()
	s.Assert().Equal(4, len(allResources), "should have 4 resources")

	wl := workloads[compName]
	s.Assert().Equal(wl.GetName(), compName, "should have same name")
	s.Assert().Equal(wl.GetNamespace(), namespace, "should have same namespace")

	labels := wl.GetLabels()
	labelKeys := make([]string, 0, len(labels))
	for k := range labels {
		labelKeys = append(labelKeys, k)
	}

	trait := traits[compName][0]
	labels = trait.GetLabels()
	labelKeys = make([]string, 0, len(labels))
	for k := range labels {
		labelKeys = append(labelKeys, k)
	}

	scaler := traits[compName][2]
	wlRef, found, err := unstructured.NestedMap(scaler.Object, "spec", "workloadRef")
	wantWorkloadRef := map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"name":       compName,
	}
	s.Assert().True(found, "should have workloadRef")
	s.Assert().Equal(wantWorkloadRef, wlRef, "should have correct workloadRef")
	scopes, err := ao.ReferencedScopes()
	wlTypedRef := corev1.ObjectReference{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       compName,
	}
	wlScope := scopes[wlTypedRef][0]
	wantScopeRef := corev1.ObjectReference{
		APIVersion: "core.oam.dev/v1beta1",
		Kind:       "HealthScope",
		Name:       "sample-health-scope",
	}
	s.Assert().Equal(wantScopeRef, wlScope, "should have correct scopeRef")
}

func (s *AssembleTestSuite) TestAssembleVela() {
	appRev := &v1beta1.ApplicationRevision{}
	b, err := os.ReadFile("./testdata/vela-apprev.yaml")
	err = yaml.Unmarshal(b, appRev)
	s.Assert().Nil(err, "should not have error when unmarshal appRevision")

	ao := NewAppManifests(appRev, appParser)
	allResources, err := ao.AssembledManifests()
	s.T().Log(len(allResources))
}
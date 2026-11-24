/*
Copyright 2026.

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

package controller

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
	// +kubebuilder:scaffold:imports
)

var (
	ctx       context.Context
	cancel    context.CancelFunc
	testEnv   *envtest.Environment
	cfg       *rest.Config
	k8sClient client.Client

	// operatorConfig authenticates as the operator: a user that holds the
	// ClusterRole shipped in config/rbac/role.yaml and nothing else. The
	// manager of this suite runs with it, so every spec also proves that
	// the shipped permissions are enough.
	operatorConfig *rest.Config
	// managerCache is the cache of that manager, for specs about what it
	// keeps in memory.
	managerCache cache.Cache
	// leftBehindBeforeStart is a monitor that already existed, in the state
	// an old operator release left it, when the manager started.
	leftBehindBeforeStart types.NamespacedName
)

const (
	// operatorUser is the user the manager runs as.
	operatorUser = "pgpilot-operator-under-test"

	// Short intervals so specs about Secrets do not have to wait for the
	// production defaults (thirty seconds and five minutes).
	testCredentialsRetry   = time.Second
	testCredentialsRecheck = 2 * time.Second
)

// operatorRBAC creates the generated ClusterRole of the operator and binds it
// to operatorUser, in the API server c talks to.
func operatorRBAC(c client.Client) {
	GinkgoHelper()
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
	Expect(err).NotTo(HaveOccurred())
	role := &rbacv1.ClusterRole{}
	Expect(yaml.Unmarshal(data, role)).To(Succeed())
	Expect(role.Rules).NotTo(BeEmpty(), "config/rbac/role.yaml holds no rules")
	Expect(c.Create(context.TODO(), role)).To(Succeed())

	binding := &rbacv1.ClusterRoleBinding{
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects: []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: operatorUser}},
	}
	binding.Name = "operator-under-test"
	Expect(c.Create(context.TODO(), binding)).To(Succeed())
}

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.TODO())

	var err error
	err = pgpilotv1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	// +kubebuilder:scaffold:scheme

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	// `make test` exports KUBEBUILDER_ASSETS for the Kubernetes version of
	// go.mod, and that choice must win. Only when it is not set (a run from
	// an IDE or a bare `go test`) fall back to what `make setup-envtest`
	// left under bin/k8s.
	if dir := envtestBinaryDir(envtestAssetsFromEnv(), filepath.Join("..", "..", "bin", "k8s")); dir != "" {
		testEnv.BinaryAssetsDirectory = dir
	}

	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	By("running the manager with the shipped RBAC and the production cache options")
	operatorRBAC(k8sClient)
	// A config without limits would get client-go's default of five
	// requests per second; the real operator runs without client-side
	// throttling, and envtest gives its own admin config the same room.
	operator, err := testEnv.AddUser(envtest.User{Name: operatorUser}, &rest.Config{QPS: 1000, Burst: 2000})
	Expect(err).NotTo(HaveOccurred())
	operatorConfig = operator.Config()

	// A monitor as operator releases up to v1.0.2 left it, stored before the
	// manager starts: the spec about it proves that the manager itself, set
	// up the way main sets it up, moves such monitors to its pgwatch release.
	legacyNamespace := &corev1.Namespace{}
	legacyNamespace.GenerateName = "test-left-behind-"
	Expect(k8sClient.Create(context.TODO(), legacyNamespace)).To(Succeed())
	leftBehindBeforeStart = types.NamespacedName{Name: "left-behind", Namespace: legacyNamespace.Name}
	legacySecret := &corev1.Secret{StringData: map[string]string{"username": "pgwatch", "password": "secret"}}
	legacySecret.Name, legacySecret.Namespace = "pg-creds", legacyNamespace.Name
	Expect(k8sClient.Create(context.TODO(), legacySecret)).To(Succeed())
	legacyMonitor := withSpec(minimalMonitor(legacyNamespace.Name, leftBehindBeforeStart.Name), legacyImageBlock(), "image")
	legacyMonitor.SetFinalizers([]string{finalizerName})
	Expect(legacyOperatorClient(cfg).Create(context.TODO(), legacyMonitor)).To(Succeed())

	// Start a real manager so reconcile loops run. It is built the way
	// main builds it: same cache and client settings, same controller setup.
	options := Options{
		CredentialsRetry:   testCredentialsRetry,
		CredentialsRecheck: testCredentialsRecheck,
	}
	mgr, err := ctrl.NewManager(operatorConfig, ManagerOptions(ctrl.Options{
		Scheme: scheme.Scheme,
		// No metrics listener: a fixed port would collide with anything
		// else on the machine, including a second run of this suite.
		Metrics: metricsserver.Options{BindAddress: "0"},
	}, options))
	Expect(err).NotTo(HaveOccurred())
	managerCache = mgr.GetCache()

	Expect(SetupWithManager(mgr, options)).To(Succeed())

	go func() {
		defer GinkgoRecover()
		Expect(mgr.Start(ctx)).To(Succeed())
	}()
})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	cancel()
	Eventually(func() error {
		return testEnv.Stop()
	}, time.Minute, time.Second).Should(Succeed())
})

// envtestAssetsFromEnv returns the directory `make test` selected.
func envtestAssetsFromEnv() string {
	return os.Getenv("KUBEBUILDER_ASSETS")
}

// envtestBinaryDir decides which directory to hand to envtest explicitly. It
// returns "" when KUBEBUILDER_ASSETS is set - envtest reads the variable
// itself, and overriding it would run the suite against another Kubernetes
// version than the one requested - and otherwise the newest version found
// under basePath.
func envtestBinaryDir(assetsFromEnv, basePath string) string {
	if assetsFromEnv != "" {
		return ""
	}
	return newestEnvTestBinaryDir(basePath)
}

// newestEnvTestBinaryDir returns the directory under basePath that holds the
// envtest binaries of the highest Kubernetes version, or "" when there is
// none. setup-envtest names them "<version>-<os>-<arch>"; several accumulate
// once the Kubernetes libraries have been upgraded, and the tests have to run
// against the newest one, not whichever sorts first.
func newestEnvTestBinaryDir(basePath string) string {
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return ""
	}
	best, bestVersion := "", []int(nil)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		version := leadingVersion(entry.Name())
		if version == nil {
			continue
		}
		if best == "" || slices.Compare(version, bestVersion) > 0 {
			best, bestVersion = entry.Name(), version
		}
	}
	if best == "" {
		return ""
	}
	return filepath.Join(basePath, best)
}

// leadingVersion parses the dotted number a name starts with ("1.37.0" in
// "1.37.0-linux-amd64"). It returns nil when the name does not start with one.
func leadingVersion(name string) []int {
	prefix, _, _ := strings.Cut(name, "-")
	var version []int
	for part := range strings.SplitSeq(prefix, ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil
		}
		version = append(version, n)
	}
	return version
}

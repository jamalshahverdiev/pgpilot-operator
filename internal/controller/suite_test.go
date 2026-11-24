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

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
	// +kubebuilder:scaffold:imports
)

var (
	ctx       context.Context
	cancel    context.CancelFunc
	testEnv   *envtest.Environment
	cfg       *rest.Config
	k8sClient client.Client
)

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

	// Start a real manager so reconcile loops run.
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme.Scheme,
	})
	Expect(err).NotTo(HaveOccurred())

	err = (&PgpilotMonitorReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("pgpilot-monitor-test"), //nolint:staticcheck // SA1019
	}).SetupWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	err = (&PgpilotMetricLibraryReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

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

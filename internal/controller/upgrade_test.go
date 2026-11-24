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
	"path/filepath"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
)

// A cluster that upgrades the CRDs already holds objects, and some of them
// break a rule that did not exist when they were stored. This spec starts an
// API server with the CRDs of v1.0.2, stores such objects, installs the
// current CRDs over them and checks what the operator can still do with them.
//
// It runs in an API server of its own, without a manager: the reconciler is
// called directly, so nothing but the step under test touches the objects.
var _ = Describe("Upgrading the CRDs over existing objects", Ordered, func() {
	var (
		env        *envtest.Environment
		c          client.Client
		reconciler *PgpilotMonitorReconciler
		ns         string
		// What the API server tolerates for an object whose stored spec
		// breaks a rule, as long as the offending value is not changed:
		// from Kubernetes 1.30 updates of the object itself, from 1.33 also
		// writes to its status subresource. Older servers validate the
		// whole object on every write.
		ratchetsUpdates, ratchetsStatus bool
	)

	// stale lists monitors that were valid under the old CRDs. Each breaks
	// one of the new rules, written the way a user would have written it.
	stale := map[string]func(spec map[string]any){
		"sqls-wildcard": func(spec map[string]any) {
			spec["metrics"] = map[string]any{"custom": []any{map[string]any{
				"name": "queue_depth", "sqls": map[string]any{"14": "SELECT 1", "*": "SELECT 1"},
			}}}
		},
		"both-roles": func(spec map[string]any) {
			spec["metrics"] = map[string]any{"custom": []any{map[string]any{
				"name": "lag", "sqls": map[string]any{"14": "SELECT 1"},
				"masterOnly": true, "standbyOnly": true, "isInstanceLevel": false,
			}}}
		},
		// Explicit zero values are what makes a full-object update differ
		// from the stored object: the Go struct drops them.
		"no-sink": func(spec map[string]any) {
			spec["sinks"] = map[string]any{
				"prometheus": map[string]any{"enabled": false},
				"grpc":       map[string]any{"enabled": false, "endpoint": ""},
			}
		},
		"no-metrics": func(spec map[string]any) {
			spec["metrics"] = map[string]any{"preset": "", "custom": []any{}}
		},
		"reserved-label": func(spec map[string]any) {
			spec["podMetadata"] = map[string]any{"labels": map[string]any{"pgpilot.io/team": "data"}}
		},
		"reserved-tag": func(spec map[string]any) {
			spec["database"].(map[string]any)["customTags"] = map[string]any{"dbname": "mine"}
		},
		"duplicate-metrics": func(spec map[string]any) {
			metric := map[string]any{"name": "twice", "sqls": map[string]any{"14": "SELECT 1"}}
			spec["metrics"] = map[string]any{"custom": []any{metric, metric}}
		},
	}

	staleMonitor := func(name string) *unstructured.Unstructured {
		spec := map[string]any{
			"database": map[string]any{
				"host": "pg.example.com", "database": "appdb",
				"credentialsSecret": map[string]any{"name": "pg-creds"},
			},
			"metrics": map[string]any{"preset": "basic"},
			"sinks":   map[string]any{"prometheus": map[string]any{"enabled": true}},
		}
		stale[name](spec)
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": pgpilotv1.GroupVersion.String(),
			"kind":       "PgpilotMonitor",
			"metadata": map[string]any{
				"name": name, "namespace": ns,
				// As left behind by the operator that reconciled it.
				"finalizers": []any{finalizerName},
			},
			"spec": spec,
		}}
	}

	BeforeAll(func() {
		env = &envtest.Environment{
			CRDDirectoryPaths:     []string{filepath.Join("testdata", "crds-v1.0.2")},
			ErrorIfCRDPathMissing: true,
		}
		if dir := envtestBinaryDir(envtestAssetsFromEnv(), filepath.Join("..", "..", "bin", "k8s")); dir != "" {
			env.BinaryAssetsDirectory = dir
		}
		restConfig, err := env.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(env.Stop()).To(Succeed()) })

		c, err = client.New(restConfig, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())
		reconciler = &PgpilotMonitorReconciler{
			Client:   c,
			Scheme:   scheme.Scheme,
			Recorder: record.NewFakeRecorder(1000),
		}

		dc, err := discovery.NewDiscoveryClientForConfig(restConfig)
		Expect(err).NotTo(HaveOccurred())
		version, err := dc.ServerVersion()
		Expect(err).NotTo(HaveOccurred())
		minor, err := strconv.Atoi(strings.TrimRight(version.Minor, "+"))
		Expect(err).NotTo(HaveOccurred())
		ratchetsUpdates = minor >= 30
		ratchetsStatus = minor >= 33

		created := &corev1.Namespace{}
		created.GenerateName = "test-upgrade-"
		Expect(c.Create(context.TODO(), created)).To(Succeed())
		ns = created.Name

		By("storing objects under the v1.0.2 CRDs")
		for name := range stale {
			Expect(c.Create(context.TODO(), staleMonitor(name))).To(Succeed(),
				"%s must be valid under the old CRDs, otherwise it proves nothing", name)
		}

		By("installing the current CRDs over them")
		_, err = envtest.InstallCRDs(restConfig, envtest.CRDInstallOptions{
			Paths:              []string{filepath.Join("..", "..", "config", "crd", "bases")},
			ErrorIfPathMissing: true,
		})
		Expect(err).NotTo(HaveOccurred())

		// The API server swaps schemas asynchronously; wait until the new
		// rules are in force.
		Eventually(func(g Gomega) {
			probe := staleMonitor("sqls-wildcard")
			probe.SetName("probe")
			err := c.Create(context.TODO(), probe, client.DryRunAll)
			g.Expect(apierrors.IsInvalid(err)).To(BeTrue(), "new rules not active yet: %v", err)
		}, timeout, interval).Should(Succeed())
	})

	It("rejects the same objects when they are created anew", func() {
		for name := range stale {
			fresh := staleMonitor(name)
			fresh.SetName("new-" + name)
			err := c.Create(context.TODO(), fresh)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%s: want an Invalid error, got: %v", name, err)
		}
	})

	It("still reads them with the typed client", func() {
		var list pgpilotv1.PgpilotMonitorList
		Expect(c.List(context.TODO(), &list, client.InNamespace(ns))).To(Succeed())
		Expect(list.Items).To(HaveLen(len(stale)))
	})

	// The operator writes status on every reconcile. Before Kubernetes 1.33
	// that write is refused for an object breaking a rule, even though the
	// spec did not change.
	It("writes their status where the API server ratchets status updates", func() {
		for name := range stale {
			var monitor pgpilotv1.PgpilotMonitor
			Expect(c.Get(context.TODO(), types.NamespacedName{Name: name, Namespace: ns}, &monitor)).To(Succeed())
			monitor.Status.ConfigHash = "after-upgrade"
			err := c.Status().Update(context.TODO(), &monitor)

			if ratchetsStatus {
				Expect(err).NotTo(HaveOccurred(), "%s: status update must be accepted", name)
			} else {
				Expect(apierrors.IsInvalid(err)).To(BeTrue(),
					"%s: this Kubernetes version is expected to refuse the status update, got: %v", name, err)
			}
		}
	})

	// A full Update of the object re-serialises the spec, which the API
	// server sees as a change and validates again: the finalizer could not
	// be removed and the object stayed in Terminating for good.
	It("lets every one of them be deleted", func() {
		for name := range stale {
			By(name)
			key := types.NamespacedName{Name: name, Namespace: ns}
			var monitor pgpilotv1.PgpilotMonitor
			Expect(c.Get(context.TODO(), key, &monitor)).To(Succeed())
			Expect(c.Delete(context.TODO(), &monitor)).To(Succeed())

			Expect(c.Get(context.TODO(), key, &monitor)).To(Succeed())
			Expect(monitor.DeletionTimestamp.IsZero()).To(BeFalse(), "the finalizer must hold the object")

			_, err := reconciler.Reconcile(context.TODO(), ctrl.Request{NamespacedName: key})
			if !ratchetsUpdates {
				// Kubernetes 1.28 and 1.29 refuse every write to such an
				// object. It stays in Terminating until its spec is fixed,
				// which the upgrade notes say.
				Expect(apierrors.IsInvalid(err)).To(BeTrue(),
					"this Kubernetes version is expected to refuse the finalizer removal, got: %v", err)
				continue
			}
			Expect(err).NotTo(HaveOccurred(), "releasing the finalizer must not be refused")

			err = c.Get(context.TODO(), key, &monitor)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the object must be gone, got: %v", err)
		}
	})
})

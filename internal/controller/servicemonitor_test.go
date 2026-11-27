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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
	"github.com/jamalshahverdiev/pgpilot-operator/internal/builder"
)

// Clusters that run prometheus-operator are the common case, and on them the
// operator also creates, watches and caches ServiceMonitors. The main suite
// runs without that API, so this one starts an API server that serves it and
// a manager with ServiceMonitors enabled - again as the user holding only the
// shipped role, and built through ManagerOptions and SetupWithManager.
var _ = Describe("With the ServiceMonitor API present", Ordered, func() {
	var (
		admin        client.Client
		monitorCache cache.Cache
		ns           string
	)

	BeforeAll(func() {
		Expect(monitoringv1.AddToScheme(scheme.Scheme)).To(Succeed())

		env := &envtest.Environment{
			CRDDirectoryPaths: []string{
				filepath.Join("..", "..", "config", "crd", "bases"),
				filepath.Join("testdata", "crds-servicemonitor"),
			},
			ErrorIfCRDPathMissing: true,
		}
		if dir := envtestBinaryDir(envtestAssetsFromEnv(), filepath.Join("..", "..", "bin", "k8s")); dir != "" {
			env.BinaryAssetsDirectory = dir
		}
		adminConfig, err := env.Start()
		Expect(err).NotTo(HaveOccurred())

		admin, err = client.New(adminConfig, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())
		operatorRBAC(admin)
		operator, err := env.AddUser(envtest.User{Name: operatorUser}, &rest.Config{QPS: 1000, Burst: 2000})
		Expect(err).NotTo(HaveOccurred())

		options := Options{ServiceMonitorEnabled: true, CredentialsRetry: testCredentialsRetry}
		mgr, err := ctrl.NewManager(operator.Config(), ManagerOptions(ctrl.Options{
			Scheme:  scheme.Scheme,
			Metrics: metricsserver.Options{BindAddress: "0"},
			// A second manager in one process: its controllers would
			// otherwise collide with the names the main suite registered.
			Controller: config.Controller{SkipNameValidation: new(true)},
		}, options))
		Expect(err).NotTo(HaveOccurred())
		Expect(SetupWithManager(mgr, options)).To(Succeed())
		monitorCache = mgr.GetCache()

		managerContext, stop := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			defer close(done)
			Expect(mgr.Start(managerContext)).To(Succeed())
		}()
		DeferCleanup(func() {
			stop()
			Eventually(done, timeout).Should(BeClosed())
			Expect(env.Stop()).To(Succeed())
		})

		created := &corev1.Namespace{}
		created.GenerateName = "test-servicemonitor-"
		Expect(admin.Create(context.TODO(), created)).To(Succeed())
		ns = created.Name

		// The monitor all specs of this container are about. Created here,
		// so that each of them can be run on its own.
		secret := &corev1.Secret{StringData: map[string]string{"username": "pgwatch", "password": "s3cret"}}
		secret.Name, secret.Namespace = "db-creds", ns
		Expect(admin.Create(context.TODO(), secret)).To(Succeed())

		monitor := &pgpilotv1.PgpilotMonitor{}
		monitor.Name, monitor.Namespace = "orders", ns
		monitor.Spec.Database = pgpilotv1.DatabaseRef{
			Host: "pg.example.com", Database: "appdb",
			CredentialsSecret: &pgpilotv1.CredentialsSecretRef{Name: "db-creds"},
		}
		monitor.Spec.Metrics = pgpilotv1.MetricsSpec{Preset: "basic"}
		monitor.Spec.Sinks = pgpilotv1.SinksSpec{Prometheus: &pgpilotv1.PrometheusSink{Enabled: true}}
		Expect(admin.Create(context.TODO(), monitor)).To(Succeed())
	})

	It("creates a ServiceMonitor for a monitor, owned by it", func() {
		Eventually(func(g Gomega) {
			var sm monitoringv1.ServiceMonitor
			g.Expect(admin.Get(context.TODO(), types.NamespacedName{Name: "pgpilot-orders", Namespace: ns}, &sm)).To(Succeed())
			g.Expect(sm.Labels).To(HaveKeyWithValue(builder.LabelAppManagedBy, builder.ManagedBy))
			g.Expect(sm.Spec.Endpoints).To(HaveLen(1))
			g.Expect(sm.Spec.Endpoints[0].Port).To(Equal("metrics"))
			g.Expect(sm.OwnerReferences).To(HaveLen(1))
			g.Expect(sm.OwnerReferences[0].Kind).To(Equal("PgpilotMonitor"))
			g.Expect(sm.OwnerReferences[0].Name).To(Equal("orders"))
			g.Expect(sm.OwnerReferences[0].Controller).To(HaveValue(BeTrue()))
		}, timeout, interval).Should(Succeed())

		// The reconcile went all the way through: status is written only
		// after every object, the ServiceMonitor included, was applied.
		Eventually(func(g Gomega) {
			var m pgpilotv1.PgpilotMonitor
			g.Expect(admin.Get(context.TODO(), types.NamespacedName{Name: "orders", Namespace: ns}, &m)).To(Succeed())
			g.Expect(m.Status.ObservedGeneration).To(Equal(m.Generation))
			g.Expect(m.Status.ConfigHash).NotTo(BeEmpty())
		}, timeout, interval).Should(Succeed())
	})

	// The watch on ServiceMonitors is what notices this.
	It("puts back a ServiceMonitor that was changed by hand", func() {
		key := types.NamespacedName{Name: "pgpilot-orders", Namespace: ns}
		Eventually(func(g Gomega) {
			var sm monitoringv1.ServiceMonitor
			g.Expect(admin.Get(context.TODO(), key, &sm)).To(Succeed())
			sm.Spec.Endpoints[0].Port = "tampered"
			g.Expect(admin.Update(context.TODO(), &sm)).To(Succeed())
		}, timeout, interval).Should(Succeed())

		Eventually(func(g Gomega) {
			var sm monitoringv1.ServiceMonitor
			g.Expect(admin.Get(context.TODO(), key, &sm)).To(Succeed())
			g.Expect(sm.Spec.Endpoints[0].Port).To(Equal("metrics"))
		}, timeout, interval).Should(Succeed())
	})

	serviceMonitorCondition := func(g Gomega) *metav1.Condition {
		var m pgpilotv1.PgpilotMonitor
		g.Expect(admin.Get(context.TODO(), types.NamespacedName{Name: "orders", Namespace: ns}, &m)).To(Succeed())
		g.Expect(m.Status.ObservedGeneration).To(Equal(m.Generation))
		return meta.FindStatusCondition(m.Status.Conditions, "ServiceMonitorApplied")
	}
	patchMonitor := func(patch string) {
		GinkgoHelper()
		obj := &pgpilotv1.PgpilotMonitor{}
		obj.Name, obj.Namespace = "orders", ns
		Expect(admin.Patch(context.TODO(), obj, client.RawPatch(types.MergePatchType, []byte(patch)))).To(Succeed())
	}

	It("says in a condition that the ServiceMonitor is in place", func() {
		Eventually(func(g Gomega) {
			cond := serviceMonitorCondition(g)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(cond.Reason).To(Equal("Applied"))
		}, timeout, interval).Should(Succeed())
	})

	// Service and ServiceMonitor point at the port of the Prometheus sink.
	It("removes Service and ServiceMonitor when the Prometheus sink is turned off, and brings them back", func() {
		key := types.NamespacedName{Name: "pgpilot-orders", Namespace: ns}
		gone := func(obj client.Object) func() bool {
			return func() bool { return apierrors.IsNotFound(admin.Get(context.TODO(), key, obj)) }
		}

		patchMonitor(`{"spec":{"sinks":{"prometheus":{"enabled":false},"grpc":{"enabled":true,"endpoint":"receiver.example.com:5000"}}}}`)
		Eventually(gone(&monitoringv1.ServiceMonitor{}), timeout, interval).Should(BeTrue())
		Eventually(gone(&corev1.Service{}), timeout, interval).Should(BeTrue())
		Eventually(func(g Gomega) {
			g.Expect(serviceMonitorCondition(g)).To(BeNil(), "a monitor that is to have no ServiceMonitor reports on none")
		}, timeout, interval).Should(Succeed())

		patchMonitor(`{"spec":{"sinks":{"prometheus":{"enabled":true}}}}`)
		Eventually(gone(&monitoringv1.ServiceMonitor{}), timeout, interval).Should(BeFalse())
		Eventually(gone(&corev1.Service{}), timeout, interval).Should(BeFalse())
		Eventually(func(g Gomega) {
			cond := serviceMonitorCondition(g)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		}, timeout, interval).Should(Succeed())
	})

	It("keeps only its own ServiceMonitors in the cache", func() {
		foreign := &monitoringv1.ServiceMonitor{}
		foreign.Name, foreign.Namespace = "someone-elses", ns
		foreign.Spec.Selector = metav1.LabelSelector{MatchLabels: map[string]string{"app": "other"}}
		foreign.Spec.Endpoints = []monitoringv1.Endpoint{{Port: "http"}}
		Expect(admin.Create(context.TODO(), foreign)).To(Succeed())

		Eventually(func(g Gomega) {
			var cached monitoringv1.ServiceMonitorList
			g.Expect(monitorCache.List(context.TODO(), &cached, client.InNamespace(ns))).To(Succeed())
			names := make([]string, 0, len(cached.Items))
			for _, sm := range cached.Items {
				names = append(names, sm.Name)
			}
			g.Expect(names).To(ConsistOf("pgpilot-orders"))
		}, timeout, interval).Should(Succeed())
	})

	// The operator decides when it starts whether the cluster has
	// ServiceMonitors. Uninstalling the prometheus-operator later made every
	// reconcile fail before it wrote a status, for something the collector
	// does not need. Last spec of this container: it takes the API away.
	It("goes on reconciling when the cluster stops serving ServiceMonitors", func() {
		crd := &unstructured.Unstructured{}
		crd.SetAPIVersion("apiextensions.k8s.io/v1")
		crd.SetKind("CustomResourceDefinition")
		crd.SetName("servicemonitors.monitoring.coreos.com")
		Expect(admin.Delete(context.TODO(), crd)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(admin.Get(context.TODO(), client.ObjectKeyFromObject(crd), crd))
		}, timeout, interval).Should(BeTrue())

		// A change of the spec that has to reach the collector all the same.
		patchMonitor(`{"spec":{"database":{"host":"pg-new.example.com"}}}`)

		Eventually(func(g Gomega) {
			cond := serviceMonitorCondition(g)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse), cond.Message)
			g.Expect(cond.Reason).To(Equal("ServiceMonitorAPIMissing"))
			g.Expect(cond.Message).To(ContainSubstring("no longer serves monitoring.coreos.com/v1"))
		}, timeout, interval).Should(Succeed())

		Eventually(func(g Gomega) {
			cm, err := mountedConfigMap(admin, types.NamespacedName{Name: "orders", Namespace: ns})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cm.Data[builder.SourcesKey]).To(ContainSubstring("pg-new.example.com"))
		}, timeout, interval).Should(Succeed())

		var m pgpilotv1.PgpilotMonitor
		Expect(admin.Get(context.TODO(), types.NamespacedName{Name: "orders", Namespace: ns}, &m)).To(Succeed())
		ready := meta.FindStatusCondition(m.Status.Conditions, "Ready")
		Expect(ready).NotTo(BeNil())
		Expect(ready.Reason).To(Equal("RolloutInProgress"), "the collector is rolled out as if nothing had happened: %s", ready.Message)

		Eventually(func(g Gomega) {
			var events corev1.EventList
			g.Expect(admin.List(context.TODO(), &events, client.InNamespace(ns))).To(Succeed())
			g.Expect(events.Items).To(ContainElement(SatisfyAll(
				HaveField("InvolvedObject.Name", "orders"),
				HaveField("Reason", "ServiceMonitorAPIMissing"),
				HaveField("Type", corev1.EventTypeWarning),
			)))
		}, timeout, interval).Should(Succeed())
	})
})

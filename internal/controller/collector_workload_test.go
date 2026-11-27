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
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
)

// The API server refuses a monitor without a sink. One stored before it did
// still reaches the operator, and so does a gRPC sink that names no endpoint.
func TestReconcile_AMonitorWithoutASinkGetsNoCollector(t *testing.T) {
	cases := map[string]pgpilotv1.SinksSpec{
		"nothing set":                        {},
		"the Prometheus sink turned off":     {Prometheus: &pgpilotv1.PrometheusSink{Enabled: false, Port: 9187}},
		"both turned off":                    {Prometheus: &pgpilotv1.PrometheusSink{}, GRPC: &pgpilotv1.GRPCSink{Endpoint: "receiver:5000"}},
		"a gRPC sink that names no endpoint": {GRPC: &pgpilotv1.GRPCSink{Enabled: true}},
	}
	for name, sinks := range cases {
		t.Run(name, func(t *testing.T) {
			monitor := unitMonitor()
			monitor.Spec.Sinks = sinks
			h := newReconcileHarness(t, monitor, unitSecret())

			result, err := h.reconcile()
			// A mistake in the spec: trying again cannot fix it.
			if err != nil || result != (ctrl.Result{}) {
				t.Errorf("got %+v, %v; want neither an error nor another look", result, err)
			}
			if len(h.applied) != 0 {
				t.Errorf("applied %v for a collector that can only exit", h.applied)
			}
			h.expectCondition("Ready", metav1.ConditionFalse, "NoSinkEnabled", "No sink is in use")
			h.expectCondition("Ready", metav1.ConditionFalse, "NoSinkEnabled", "nothing is changed for this monitor until one is enabled")
			// Both halves: which one a user has to look at depends on the
			// sink they meant to use.
			for _, part := range []string{"spec.sinks.prometheus is not enabled", "spec.sinks.grpc is not enabled or names no endpoint"} {
				h.expectCondition("Ready", metav1.ConditionFalse, "NoSinkEnabled", part)
				h.expectCondition("ConfigGenerated", metav1.ConditionFalse, "NoSinkEnabled", part)
			}
			if events := drain(h.recorder); len(events) != 1 || !strings.HasPrefix(events[0], "Warning NoSinkEnabled No sink is in use") {
				t.Errorf("events: %q", events)
			}

			writes := h.statusWrites
			if _, err := h.reconcile(); err != nil {
				t.Fatal(err)
			}
			if again := drain(h.recorder); len(again) != 0 || h.statusWrites != writes {
				t.Errorf("reported again: events %q, %d status writes", again, h.statusWrites-writes)
			}
		})
	}

	// Only a monitor stored by an earlier release can be without a sink,
	// and that release made a Deployment and a Service for it. They are left
	// as they are, like everything else of a monitor whose spec is at fault.
	t.Run("what an earlier release generated for it is left as it is", func(t *testing.T) {
		monitor := unitMonitor()
		monitor.Spec.Sinks = pgpilotv1.SinksSpec{Prometheus: &pgpilotv1.PrometheusSink{Enabled: false}}
		left := &appsv1.Deployment{}
		left.Name, left.Namespace = "pgpilot-db", "team"
		service := &corev1.Service{}
		service.Name, service.Namespace = "pgpilot-db", "team"
		h := newReconcileHarness(t, monitor, unitSecret(), left, service)

		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		h.expectCondition("Ready", metav1.ConditionFalse, "NoSinkEnabled", "")
		if len(h.applied) != 0 || len(h.deleted) != 0 {
			t.Errorf("applied %v, deleted %v", h.applied, h.deleted)
		}
		key := types.NamespacedName{Namespace: "team", Name: "pgpilot-db"}
		for _, obj := range []client.Object{&appsv1.Deployment{}, &corev1.Service{}} {
			if err := h.client.Get(context.Background(), key, obj); err != nil {
				t.Errorf("%T: %v", obj, err)
			}
		}
	})

	t.Run("one sink is enough", func(t *testing.T) {
		for name, sinks := range map[string]pgpilotv1.SinksSpec{
			"Prometheus": {Prometheus: &pgpilotv1.PrometheusSink{Enabled: true}, GRPC: &pgpilotv1.GRPCSink{Enabled: true}},
			"gRPC":       {Prometheus: &pgpilotv1.PrometheusSink{}, GRPC: &pgpilotv1.GRPCSink{Enabled: true, Endpoint: "receiver:5000"}},
		} {
			monitor := unitMonitor()
			monitor.Spec.Sinks = sinks
			h := newReconcileHarness(t, monitor, unitSecret())
			if _, err := h.reconcile(); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(strings.Join(h.applied, ","), "ConfigMap,Deployment") {
				t.Errorf("%s alone: applied %v", name, h.applied)
			}
		}
	})

	t.Run("it is said before the credentials are looked at, and corrected by enabling a sink", func(t *testing.T) {
		monitor := unitMonitor()
		monitor.Spec.Sinks = pgpilotv1.SinksSpec{}
		h := newReconcileHarness(t, monitor) // no Secret
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		h.expectCondition("Ready", metav1.ConditionFalse, "NoSinkEnabled", "")
		h.expectCondition("CredentialsAvailable", metav1.ConditionUnknown, "NoSinkEnabled", "Not evaluated")

		stored := h.monitor()
		stored.Spec.Sinks = pgpilotv1.SinksSpec{GRPC: &pgpilotv1.GRPCSink{Enabled: true, Endpoint: "receiver:5000"}}
		stored.Generation++
		if err := h.client.Update(context.Background(), stored); err != nil {
			t.Fatal(err)
		}
		if err := h.client.Create(context.Background(), unitSecret()); err != nil {
			t.Fatal(err)
		}
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(h.applied, ","); got != "ConfigMap,Deployment" {
			t.Errorf("applied %q", got)
		}
	})
}

// A collector that only sends to a gRPC receiver listens nowhere. With a
// port and probes of the Prometheus sink its pod never became ready and was
// restarted every minute.
func TestReconcile_ACollectorWithoutThePrometheusSinkIsNotProbed(t *testing.T) {
	collector := func(h *reconcileHarness) (*appsv1.Deployment, corev1.Container) {
		t.Helper()
		var dep appsv1.Deployment
		if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: "team", Name: "pgpilot-db"}, &dep); err != nil {
			t.Fatal(err)
		}
		return &dep, dep.Spec.Template.Spec.Containers[0]
	}

	h := newReconcileHarness(t, unitMonitor(), unitSecret())
	if _, err := h.reconcile(); err != nil {
		t.Fatal(err)
	}
	if dep, c := collector(h); len(c.Ports) != 1 || c.LivenessProbe == nil || c.ReadinessProbe == nil || dep.Spec.MinReadySeconds != 0 {
		t.Fatalf("with the Prometheus sink: ports %v, liveness %v, readiness %v, minReadySeconds %d",
			c.Ports, c.LivenessProbe, c.ReadinessProbe, dep.Spec.MinReadySeconds)
	}

	grpcOnly(h)
	if _, err := h.reconcile(); err != nil {
		t.Fatal(err)
	}
	dep, c := collector(h)
	if len(c.Ports) != 0 || c.LivenessProbe != nil || c.ReadinessProbe != nil {
		t.Errorf("without it: ports %v, liveness %v, readiness %v", c.Ports, c.LivenessProbe, c.ReadinessProbe)
	}
	if dep.Spec.MinReadySeconds != 60 {
		t.Errorf("minReadySeconds %d, want 60: a collector that gives up on its sink must not count as running before",
			dep.Spec.MinReadySeconds)
	}
}

var _ = Describe("The collector pod", func() {
	var ns string

	collectorOf := func(key types.NamespacedName) func(g Gomega) *appsv1.Deployment {
		return func(g Gomega) *appsv1.Deployment {
			var dep appsv1.Deployment
			g.Expect(k8sClient.Get(context.TODO(),
				types.NamespacedName{Name: "pgpilot-" + key.Name, Namespace: key.Namespace}, &dep)).To(Succeed())
			g.Expect(dep.Spec.Template.Spec.Containers).To(HaveLen(1))
			return &dep
		}
	}
	grpcSink := map[string]any{
		"prometheus": map[string]any{"enabled": false},
		"grpc":       map[string]any{"enabled": true, "endpoint": "receiver.monitoring:9000"},
	}

	BeforeEach(func() {
		namespace := &corev1.Namespace{}
		namespace.GenerateName = "test-workload-"
		// What a platform team sets on tenant namespaces.
		namespace.Labels = map[string]string{
			"pod-security.kubernetes.io/enforce":         "restricted",
			"pod-security.kubernetes.io/enforce-version": "latest",
		}
		Expect(k8sClient.Create(context.TODO(), namespace)).To(Succeed())
		ns = namespace.Name
		secret := &corev1.Secret{StringData: map[string]string{"username": "pgwatch", "password": "secret"}}
		secret.Name, secret.Namespace = "pg-creds", ns
		createForSpec(secret)
	})

	// Nothing creates pods in this API server, so the pod is made here,
	// from the template, as the ReplicaSet controller would.
	podFrom := func(dep *appsv1.Deployment) *corev1.Pod {
		pod := &corev1.Pod{}
		pod.GenerateName, pod.Namespace = dep.Name+"-", dep.Namespace
		pod.Labels, pod.Annotations = dep.Spec.Template.Labels, dep.Spec.Template.Annotations
		pod.Spec = *dep.Spec.Template.Spec.DeepCopy()
		return pod
	}

	DescribeTable("is admitted in a namespace that enforces the restricted Pod Security Standard",
		func(sinks map[string]any) {
			monitor := minimalMonitor(ns, "tenant")
			if sinks != nil {
				monitor = withSpec(monitor, sinks, "sinks")
			}
			createForSpec(monitor)
			var dep *appsv1.Deployment
			Eventually(func(g Gomega) { dep = collectorOf(client.ObjectKeyFromObject(monitor))(g) }, timeout, interval).Should(Succeed())

			admitted := podFrom(dep)
			Expect(k8sClient.Create(context.TODO(), admitted)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(context.TODO(), admitted) })
			Expect(admitted.Spec.AutomountServiceAccountToken).To(HaveValue(BeFalse()))

			// The same pod without the seccomp profile is what earlier
			// releases generated. That it is refused shows that the
			// admission check is at work in this API server at all.
			without := podFrom(dep)
			without.Spec.SecurityContext.SeccompProfile = nil
			err := k8sClient.Create(context.TODO(), without)
			Expect(apierrors.IsForbidden(err)).To(BeTrue(), "got: %v", err)
			Expect(err.Error()).To(ContainSubstring("seccompProfile"))
		},
		Entry("with the Prometheus sink", nil),
		Entry("with the gRPC sink alone", grpcSink),
	)

	It("gets the port and the probes of the Prometheus sink, and loses them with it", func() {
		monitor := minimalMonitor(ns, "sinks")
		createForSpec(monitor)
		key := client.ObjectKeyFromObject(monitor)

		probed := func(g Gomega) {
			dep := collectorOf(key)(g)
			c := dep.Spec.Template.Spec.Containers[0]
			g.Expect(c.Ports).To(HaveLen(1))
			g.Expect(c.Ports[0].Name).To(Equal("metrics"))
			g.Expect(c.LivenessProbe).NotTo(BeNil())
			g.Expect(c.ReadinessProbe).NotTo(BeNil())
			g.Expect(c.ReadinessProbe.TCPSocket.Port.IntValue()).To(Equal(9187))
			g.Expect(dep.Spec.MinReadySeconds).To(BeZero())
		}
		Eventually(probed, timeout, interval).Should(Succeed())

		// The operator writes the status of the monitor after it has
		// applied the Deployment; an update that meets that write is
		// refused and made again.
		setSinks := func(sinks map[string]any) {
			GinkgoHelper()
			Eventually(func(g Gomega) {
				stored := &unstructured.Unstructured{}
				stored.SetGroupVersionKind(pgpilotv1.GroupVersion.WithKind("PgpilotMonitor"))
				g.Expect(k8sClient.Get(context.TODO(), key, stored)).To(Succeed())
				g.Expect(unstructured.SetNestedMap(stored.Object, sinks, "spec", "sinks")).To(Succeed())
				g.Expect(k8sClient.Update(context.TODO(), stored)).To(Succeed())
			}, timeout, interval).Should(Succeed())
		}

		// The fields the operator set and no longer sets have to go from
		// the stored Deployment, not only from what it applies.
		setSinks(grpcSink)
		Eventually(func(g Gomega) {
			dep := collectorOf(key)(g)
			c := dep.Spec.Template.Spec.Containers[0]
			g.Expect(c.Ports).To(BeEmpty())
			g.Expect(c.LivenessProbe).To(BeNil())
			g.Expect(c.ReadinessProbe).To(BeNil())
			g.Expect(dep.Spec.MinReadySeconds).To(BeEquivalentTo(60))
			g.Expect(c.Args).To(ContainElement("grpc://receiver.monitoring:9000"))
			g.Expect(c.Args).NotTo(ContainElement(HavePrefix("prometheus://")))
		}, timeout, interval).Should(Succeed())

		setSinks(map[string]any{"prometheus": map[string]any{"enabled": true}})
		Eventually(probed, timeout, interval).Should(Succeed())
	})

	DescribeTable("pulls its image as spec.image.pullPolicy says",
		func(image map[string]any, want corev1.PullPolicy) {
			monitor := minimalMonitor(ns, "pull")
			if image != nil {
				monitor = withSpec(monitor, image, "image")
			}
			createForSpec(monitor)
			Eventually(func(g Gomega) corev1.PullPolicy {
				return collectorOf(client.ObjectKeyFromObject(monitor))(g).Spec.Template.Spec.Containers[0].ImagePullPolicy
			}, timeout, interval).Should(Equal(want))
		},
		Entry("Always", map[string]any{"pullPolicy": "Always"}, corev1.PullAlways),
		Entry("Never", map[string]any{"pullPolicy": "Never"}, corev1.PullNever),
		Entry("IfNotPresent", map[string]any{"pullPolicy": "IfNotPresent"}, corev1.PullIfNotPresent),
		// The field has a default, IfNotPresent, and that is what the
		// monitor says then. Left to the API server, the tag "latest" would
		// be pulled at every start, against what the CRD documents.
		Entry("the default of the field, also next to the tag latest",
			map[string]any{"tag": "latest"}, corev1.PullIfNotPresent),
		// No image block at all: nothing is defaulted in the monitor, and
		// the API server decides by the tag, as for any pod.
		Entry("the API server's choice for a monitor that names no image", nil, corev1.PullIfNotPresent),
	)

	It("tells the cluster autoscaler that its emptyDir holds nothing to keep", func() {
		monitor := minimalMonitor(ns, "evictable")
		createForSpec(monitor)
		Eventually(func(g Gomega) map[string]string {
			return collectorOf(client.ObjectKeyFromObject(monitor))(g).Spec.Template.Annotations
		}, timeout, interval).Should(HaveKeyWithValue("cluster-autoscaler.kubernetes.io/safe-to-evict-local-volumes", "tmp"))
	})
})

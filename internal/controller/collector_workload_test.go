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
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	fakediscovery "k8s.io/client-go/discovery/fake"
	clienttesting "k8s.io/client-go/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
	"github.com/jamalshahverdiev/pgpilot-operator/internal/builder"
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

	// On OpenShift the pod names no user and the cluster assigns one. That
	// pod has to be one the restricted Pod Security Standard admits as
	// well: clusters enforce both.
	It("is admitted without a user of its own, which the platform then assigns", func() {
		monitor := &pgpilotv1.PgpilotMonitor{}
		monitor.Name, monitor.Namespace = "no-user", ns
		monitor.Spec.Database = pgpilotv1.DatabaseRef{
			Host: "pg.example.com", Database: "appdb", CredentialsSecret: &pgpilotv1.CredentialsSecretRef{Name: "pg-creds"},
		}
		monitor.Spec.Sinks = pgpilotv1.SinksSpec{Prometheus: &pgpilotv1.PrometheusSink{Enabled: true, Port: 9187}}
		dep := builder.BuildDeployment(monitor, builder.MergedMetrics{Preset: "basic"},
			builder.PodTemplateVersions{Config: "0123456789"}, builder.RunAsUser(nil))
		Expect(dep.Spec.Template.Spec.Containers[0].SecurityContext.RunAsUser).To(BeNil())

		admitted := podFrom(dep)
		Expect(k8sClient.Create(context.TODO(), admitted)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.TODO(), admitted) })

		// And not because nothing is checked: the same pod that may run as
		// root is refused.
		asRoot := podFrom(dep)
		asRoot.Spec.SecurityContext.RunAsNonRoot = nil
		asRoot.Spec.Containers[0].SecurityContext.RunAsNonRoot = nil
		err := k8sClient.Create(context.TODO(), asRoot)
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "got: %v", err)
		Expect(err.Error()).To(ContainSubstring("runAsNonRoot"))
	})

	It("runs where the monitor says, as the account it names, with the variables it is given", func() {
		monitor := withSpec(withSpec(withSpec(withSpec(minimalMonitor(ns, "placed"),
			map[string]any{
				"nodeSelector": map[string]any{"pool": "monitoring"},
				"tolerations":  []any{map[string]any{"key": "dedicated", "operator": "Exists", "effect": "NoSchedule"}},
				"affinity": map[string]any{"podAntiAffinity": map[string]any{
					"preferredDuringSchedulingIgnoredDuringExecution": []any{map[string]any{
						"weight": int64(50),
						"podAffinityTerm": map[string]any{
							"topologyKey":   "kubernetes.io/hostname",
							"labelSelector": map[string]any{"matchLabels": map[string]any{"app.kubernetes.io/managed-by": "pgpilot-operator"}},
						},
					}},
				}},
				"topologySpreadConstraints": []any{map[string]any{
					"maxSkew": int64(1), "topologyKey": "topology.kubernetes.io/zone", "whenUnsatisfiable": "ScheduleAnyway",
					"labelSelector": map[string]any{"matchLabels": map[string]any{"app.kubernetes.io/managed-by": "pgpilot-operator"}},
				}},
				"priorityClassName": "monitoring",
			}, "scheduling"),
			"collector", "serviceAccountName"),
			[]any{map[string]any{"name": "registry"}}, "image", "pullSecrets"),
			[]any{
				map[string]any{"name": "PGAPPNAME", "value": "pgwatch for $(PGUSER)"},
				map[string]any{"name": "HTTPS_PROXY", "valueFrom": map[string]any{
					"secretKeyRef": map[string]any{"name": "proxy", "key": "url"},
				}},
			}, "env")
		createForSpec(monitor)
		key := client.ObjectKeyFromObject(monitor)

		var pod corev1.PodSpec
		Eventually(func(g Gomega) { pod = collectorOf(key)(g).Spec.Template.Spec }, timeout, interval).Should(Succeed())
		Expect(pod.NodeSelector).To(Equal(map[string]string{"pool": "monitoring"}))
		Expect(pod.Tolerations).To(HaveLen(1))
		Expect(pod.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution).To(HaveLen(1))
		Expect(pod.TopologySpreadConstraints).To(HaveLen(1))
		Expect(pod.PriorityClassName).To(Equal("monitoring"))
		Expect(pod.ServiceAccountName).To(Equal("collector"))
		Expect(pod.AutomountServiceAccountToken).To(HaveValue(BeFalse()))
		Expect(pod.ImagePullSecrets).To(Equal([]corev1.LocalObjectReference{{Name: "registry"}}))
		env := pod.Containers[0].Env
		Expect(env).To(HaveLen(4))
		Expect([]string{env[0].Name, env[1].Name, env[2].Name, env[3].Name}).To(
			Equal([]string{"PGUSER", "PGPASSWORD", "PGAPPNAME", "HTTPS_PROXY"}))
		Expect(env[3].ValueFrom.SecretKeyRef.Name).To(Equal("proxy"))

		// Taken out of the monitor, they are gone from the pod.
		Eventually(func(g Gomega) {
			stored := &unstructured.Unstructured{}
			stored.SetGroupVersionKind(pgpilotv1.GroupVersion.WithKind("PgpilotMonitor"))
			g.Expect(k8sClient.Get(context.TODO(), key, stored)).To(Succeed())
			for _, field := range []string{"scheduling", "serviceAccountName", "env"} {
				unstructured.RemoveNestedField(stored.Object, "spec", field)
			}
			unstructured.RemoveNestedField(stored.Object, "spec", "image", "pullSecrets")
			g.Expect(k8sClient.Update(context.TODO(), stored)).To(Succeed())
		}, timeout, interval).Should(Succeed())
		Eventually(func(g Gomega) {
			pod := collectorOf(key)(g).Spec.Template.Spec
			g.Expect(pod.NodeSelector).To(BeEmpty())
			g.Expect(pod.Tolerations).To(BeEmpty())
			g.Expect(pod.Affinity).To(BeNil())
			g.Expect(pod.TopologySpreadConstraints).To(BeEmpty())
			g.Expect(pod.PriorityClassName).To(BeEmpty())
			g.Expect(pod.ImagePullSecrets).To(BeEmpty())
			g.Expect(pod.Containers[0].Env).To(HaveLen(2))
			// The API server keeps a deprecated spelling of the account
			// in step with this one, and would bring the old name back
			// from it if the operator only stopped naming one.
			g.Expect(pod.ServiceAccountName).To(Equal("default"))
			g.Expect(pod.DeprecatedServiceAccount).To(Equal("default"))
		}, timeout, interval).Should(Succeed())
	})

	// The fields of a pod are passed on as they are. What a pod cannot have
	// the API server says when the Deployment is written, and the monitor
	// repeats it.
	It("reports scheduling that no pod can have", func() {
		monitor := withSpec(minimalMonitor(ns, "impossible"),
			[]any{map[string]any{"key": "dedicated", "operator": "Sometimes"}}, "scheduling", "tolerations")
		createForSpec(monitor)
		key := client.ObjectKeyFromObject(monitor)
		Eventually(func(g Gomega) {
			var m pgpilotv1.PgpilotMonitor
			g.Expect(k8sClient.Get(context.TODO(), key, &m)).To(Succeed())
			cond := meta.FindStatusCondition(m.Status.Conditions, "Ready")
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Reason).To(Equal("ApplyRejected"), cond.Message)
			g.Expect(cond.Message).To(ContainSubstring("Deployment pgpilot-impossible"))
			g.Expect(cond.Message).To(ContainSubstring("tolerations"))
		}, timeout, interval).Should(Succeed())
	})

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

// A field of a pod that the cluster's version of Kubernetes does not have is
// refused by the API server before anything is validated, with an internal
// error. It is as final as "invalid": nothing changes until the monitor
// does, and it is reported and looked at again like one, not retried as a
// failure of the operator.
func TestReconcile_AFieldTheClusterDoesNotKnowIsARejection(t *testing.T) {
	typedPatch := &apierrors.StatusError{ErrStatus: metav1.Status{
		Status: metav1.StatusFailure, Code: 500,
		Message: "failed to create typed patch object (team/pgpilot-db; apps/v1, Kind=Deployment): " +
			".spec.template.spec.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[0]." +
			"matchLabelKeys: field not declared in schema",
	}}
	h := newReconcileHarness(t, unitMonitor(), unitSecret())
	h.applyError = func(obj client.Object) error {
		if _, isDeployment := obj.(*appsv1.Deployment); isDeployment {
			return typedPatch
		}
		return nil
	}
	result, err := h.reconcile()
	if err != nil || result.RequeueAfter < 5*time.Minute {
		t.Errorf("got %+v, %v; want no error and another look in five minutes", result, err)
	}
	h.expectCondition("Ready", metav1.ConditionFalse, "ApplyRejected", "matchLabelKeys: field not declared in schema")

	// Any other internal error is the API server's trouble, and retried.
	other := newReconcileHarness(t, unitMonitor(), unitSecret())
	boom := apierrors.NewInternalError(errors.New("etcd is on fire"))
	other.applyError = func(client.Object) error { return boom }
	if _, err := other.reconcile(); !errors.Is(err, boom) {
		t.Errorf("got %v", err)
	}
	other.expectCondition("Ready", metav1.ConditionFalse, "ApplyFailed", "")
}

// The user of the pgwatch container is a setting of the operator: who may
// run as what is a property of the cluster.
func TestParseCollectorRunAsUser(t *testing.T) {
	type want struct {
		uid   int64 // 0: no user is named
		isErr string
	}
	for name, tc := range map[string]struct {
		value     string
		openShift bool
		want      want
	}{
		"left out":                       {"", false, want{uid: 65534}},
		"auto":                           {"auto", false, want{uid: 65534}},
		"left out, on OpenShift":         {"", true, want{}},
		"auto, on OpenShift":             {"auto", true, want{}},
		"none":                           {"none", false, want{}},
		"none, on OpenShift":             {"none", true, want{}},
		"a user":                         {"1001", false, want{uid: 1001}},
		"a user, on OpenShift":           {"1000680000", true, want{uid: 1000680000}},
		"the default, said out loud":     {"65534", true, want{uid: 65534}},
		"the largest user there is":      {"2147483647", false, want{uid: 2147483647}},
		"one more than that":             {"2147483648", false, want{isErr: "use one from 1 to 2147483647"}},
		"root":                           {"0", false, want{isErr: "is root"}},
		"a negative number":              {"-1", false, want{isErr: "is not a user ID"}},
		"a name":                         {"nobody", false, want{isErr: `"nobody" is neither a user ID nor "auto" or "none"`}},
		"another spelling of none":       {"None", false, want{isErr: "neither a user ID"}},
		"a number with something on it":  {"1001:1001", false, want{isErr: "neither a user ID"}},
		"more than a number can be":      {"99999999999999999999", false, want{isErr: "neither a user ID"}},
		"a number with spaces around it": {" 1001", false, want{isErr: "neither a user ID"}},
	} {
		t.Run(name, func(t *testing.T) {
			user, err := ParseCollectorRunAsUser(tc.value, tc.openShift)
			if tc.want.isErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want.isErr) {
					t.Fatalf("got %v, %v; want an error with %q", user, err, tc.want.isErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			switch got := user.RunAsUserIn("team"); {
			case tc.want.uid == 0 && got != nil:
				t.Errorf("runAsUser %d, want none", *got)
			case tc.want.uid != 0 && (got == nil || *got != tc.want.uid):
				t.Errorf("runAsUser %v, want %d", got, tc.want.uid)
			}
			if tc.want.uid != 0 && user.String() != strconv.FormatInt(tc.want.uid, 10) ||
				tc.want.uid == 0 && !strings.HasPrefix(user.String(), "assigned by the platform") {
				t.Errorf("said as %q", user.String())
			}
		})
	}

	// A reconciler that was told nothing runs the collector as the default
	// user: the zero value must not be "no user".
	if uid := (CollectorUser{}).RunAsUserIn("team"); uid == nil || *uid != 65534 {
		t.Errorf("the zero value: %v", uid)
	}
}

// OpenShift assigns a user where the admission of its security context
// constraints runs, and that is not everywhere: in a handful of namespaces
// it does not, a pod that names no user gets none, and the pgwatch image
// would run as root, which the pod refuses. There, and only when the
// operator decided for itself, the collector keeps its default user.
func TestCollectorUser_InTheNamespacesWhereOpenShiftAssignsNone(t *testing.T) {
	unassigned := []string{"default", "kube-system", "kube-public", "openshift", "openshift-infra", "openshift-node"}
	assigned := []string{"team", "openshift-monitoring", "kube-node-lease", "default-team", "my-default", ""}

	userIn := func(t *testing.T, value string, openShift bool, namespace string) *int64 {
		t.Helper()
		user, err := ParseCollectorRunAsUser(value, openShift)
		if err != nil {
			t.Fatal(err)
		}
		return user.RunAsUserIn(namespace)
	}
	for _, namespace := range unassigned {
		if uid := userIn(t, "auto", true, namespace); uid == nil || *uid != 65534 {
			t.Errorf("auto on OpenShift, in %s: %v, want 65534", namespace, uid)
		}
		// Said outright, it is what was said, there as well.
		if uid := userIn(t, "none", true, namespace); uid != nil {
			t.Errorf("none, in %s: %d", namespace, *uid)
		}
		if uid := userIn(t, "1001", true, namespace); uid == nil || *uid != 1001 {
			t.Errorf("1001, in %s: %v", namespace, uid)
		}
		// Not OpenShift: every namespace is like any other.
		if uid := userIn(t, "auto", false, namespace); uid == nil || *uid != 65534 {
			t.Errorf("auto elsewhere, in %s: %v", namespace, uid)
		}
	}
	for _, namespace := range assigned {
		if uid := userIn(t, "auto", true, namespace); uid != nil {
			t.Errorf("auto on OpenShift, in %q: %d, want none", namespace, *uid)
		}
	}
	user, _ := ParseCollectorRunAsUser("auto", true)
	if got := user.String(); got != "assigned by the platform (65534 in the namespaces where OpenShift assigns none)" {
		t.Errorf("said as %q", got)
	}
	if none, _ := ParseCollectorRunAsUser("none", true); none.String() != "assigned by the platform" {
		t.Errorf("none said as %q", none.String())
	}
}

// groupsOnly is a cluster that answers with its API groups, or does not.
type groupsOnly struct {
	*fakediscovery.FakeDiscovery
	err   error
	asked int
}

func (d *groupsOnly) ServerGroups() (*metav1.APIGroupList, error) {
	d.asked++
	if d.err != nil {
		return nil, d.err
	}
	return d.FakeDiscovery.ServerGroups()
}

func clusterServing(groupVersions ...string) *groupsOnly {
	resources := make([]*metav1.APIResourceList, 0, len(groupVersions))
	for _, gv := range groupVersions {
		resources = append(resources, &metav1.APIResourceList{GroupVersion: gv})
	}
	return &groupsOnly{FakeDiscovery: &fakediscovery.FakeDiscovery{Fake: &clienttesting.Fake{Resources: resources}}}
}

// The cluster is asked what it is only where the answer decides something.
func TestResolveCollectorUser(t *testing.T) {
	openShift := func() *groupsOnly { return clusterServing("v1", "apps/v1", "security.openshift.io/v1") }
	kubernetes := func() *groupsOnly { return clusterServing("v1", "apps/v1") }

	for name, tc := range map[string]struct {
		value   string
		cluster *groupsOnly
		in      string
		want    int64 // 0: none
		asked   int
	}{
		"left out, on Kubernetes":                  {"", kubernetes(), "team", 65534, 1},
		"auto, on Kubernetes":                      {"auto", kubernetes(), "team", 65534, 1},
		"left out, on OpenShift":                   {"", openShift(), "team", 0, 1},
		"auto, on OpenShift":                       {"auto", openShift(), "team", 0, 1},
		"auto, on OpenShift, in default":           {"auto", openShift(), "default", 65534, 1},
		"none does not ask":                        {"none", kubernetes(), "team", 0, 0},
		"a user does not ask":                      {"1001", openShift(), "team", 1001, 0},
		"a user on OpenShift, in default as well":  {"1001", openShift(), "default", 1001, 0},
		"none on OpenShift, in default as well":    {"none", openShift(), "default", 0, 0},
		"the default, said out loud, on OpenShift": {"65534", openShift(), "team", 65534, 0},
	} {
		t.Run(name, func(t *testing.T) {
			user, err := ResolveCollectorUser(tc.value, tc.cluster)
			if err != nil {
				t.Fatal(err)
			}
			switch got := user.RunAsUserIn(tc.in); {
			case tc.want == 0 && got != nil:
				t.Errorf("runAsUser %d, want none", *got)
			case tc.want != 0 && (got == nil || *got != tc.want):
				t.Errorf("runAsUser %v, want %d", got, tc.want)
			}
			if tc.cluster.asked != tc.asked {
				t.Errorf("the cluster was asked %d times, want %d", tc.cluster.asked, tc.asked)
			}
		})
	}

	// Not knowing is not "this is not OpenShift": on OpenShift every
	// collector would be given a user its namespace does not allow.
	failure := apierrors.NewServiceUnavailable("apiserver is shutting down")
	for _, value := range []string{"", "auto"} {
		cluster := kubernetes()
		cluster.err = failure
		_, err := ResolveCollectorUser(value, cluster)
		if !errors.Is(err, failure) || !strings.Contains(err.Error(), "--collector-run-as-user") {
			t.Errorf("%q: got %v; want the failure, and the way around it", value, err)
		}
	}
	// With the user named, an API server that does not answer is no
	// obstacle; a user that is none is, cluster or not.
	for _, value := range []string{"none", "1001"} {
		cluster := kubernetes()
		cluster.err = failure
		if _, err := ResolveCollectorUser(value, cluster); err != nil || cluster.asked != 0 {
			t.Errorf("%q: %v, asked %d times", value, err, cluster.asked)
		}
	}
	if _, err := ResolveCollectorUser("0", kubernetes()); err == nil || !strings.Contains(err.Error(), "is root") {
		t.Errorf("root: %v", err)
	}
}

func TestReconcile_TheCollectorRunsAsTheUserOfTheCluster(t *testing.T) {
	userOf := func(h *reconcileHarness) *int64 {
		h.t.Helper()
		if _, err := h.reconcile(); err != nil {
			h.t.Fatal(err)
		}
		return h.storedDeployment().Spec.Template.Spec.Containers[0].SecurityContext.RunAsUser
	}
	h := newReconcileHarness(t, unitMonitor(), unitSecret())
	if uid := userOf(h); uid == nil || *uid != 65534 {
		t.Errorf("by default: %v", uid)
	}

	var err error
	if h.r.CollectorRunAsUser, err = ParseCollectorRunAsUser("none", false); err != nil {
		t.Fatal(err)
	}
	if uid := userOf(h); uid != nil {
		t.Errorf("left to the platform: %d", *uid)
	}
	// The field manager gives the field up: it is gone from the object,
	// not left at what was applied before.
	if h.r.CollectorRunAsUser, err = ParseCollectorRunAsUser("1001", false); err != nil {
		t.Fatal(err)
	}
	if uid := userOf(h); uid == nil || *uid != 1001 {
		t.Errorf("a user of the operator's choosing: %v", uid)
	}

	// It goes by the namespace of the monitor: the unit monitor is in
	// "team", where OpenShift assigns a user.
	if h.r.CollectorRunAsUser, err = ParseCollectorRunAsUser("auto", true); err != nil {
		t.Fatal(err)
	}
	if uid := userOf(h); uid != nil {
		t.Errorf("auto on OpenShift, in a namespace like any other: %d", *uid)
	}
	inDefault := unitMonitor()
	inDefault.Namespace = "default"
	secret := unitSecret()
	secret.Namespace = "default"
	other := newReconcileHarness(t, inDefault, secret)
	other.r.CollectorRunAsUser = h.r.CollectorRunAsUser
	if _, err := other.reconcile(); err != nil {
		t.Fatal(err)
	}
	var dep appsv1.Deployment
	if err := other.client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pgpilot-db"}, &dep); err != nil {
		t.Fatal(err)
	}
	if uid := dep.Spec.Template.Spec.Containers[0].SecurityContext.RunAsUser; uid == nil || *uid != 65534 {
		t.Errorf("auto on OpenShift, in default: %v", uid)
	}
}

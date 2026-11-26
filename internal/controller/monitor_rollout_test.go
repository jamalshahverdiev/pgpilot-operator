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
	"fmt"
	"strings"
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
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
	"github.com/jamalshahverdiev/pgpilot-operator/internal/builder"
)

// envtest runs an API server and nothing else: no Deployment controller, no
// scheduler, no kubelet. These specs play their parts by hand - they write
// the status a Deployment would get and create the pods it would have - and
// check what the operator makes of it.
var _ = Describe("Status of a monitor", func() {
	var (
		ns     string
		nudges int
	)

	BeforeEach(func() {
		ns = testNamespace("test-status-")
		secret := &corev1.Secret{StringData: map[string]string{"username": "pgwatch", "password": "secret"}}
		secret.Name, secret.Namespace = "pg-creds", ns
		createForSpec(secret)
	})

	deploymentKey := func(key types.NamespacedName) types.NamespacedName {
		return types.NamespacedName{Name: builder.ResourceName(key.Name), Namespace: key.Namespace}
	}

	// asDeploymentController writes the status the Deployment controller
	// would, for the generation the Deployment has now.
	asDeploymentController := func(key types.NamespacedName, status appsv1.DeploymentStatus) {
		GinkgoHelper()
		Eventually(func(g Gomega) {
			var dep appsv1.Deployment
			g.Expect(k8sClient.Get(context.TODO(), deploymentKey(key), &dep)).To(Succeed())
			status.ObservedGeneration = dep.Generation
			dep.Status = status
			g.Expect(k8sClient.Status().Update(context.TODO(), &dep)).To(Succeed())
		}, timeout, interval).Should(Succeed())
	}
	available := appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
	// One replica of the old template still serving, one of the new not up.
	rollingOut := appsv1.DeploymentStatus{
		Replicas: 2, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1, UnavailableReplicas: 1,
	}

	// collectorPod creates a pod as the Deployment's ReplicaSet would, in
	// the state the kubelet would report.
	collectorPod := func(key types.NamespacedName, name string, status corev1.PodStatus) *corev1.Pod {
		GinkgoHelper()
		pod := &corev1.Pod{}
		pod.Name, pod.Namespace = name, key.Namespace
		pod.Labels = builder.SystemLabels(key.Name)
		pod.Spec.Containers = []corev1.Container{{Name: "pgwatch", Image: "cybertecpostgresql/pgwatch:6.2.0"}}
		Expect(k8sClient.Create(context.TODO(), pod)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.TODO(), pod, client.GracePeriodSeconds(0))
		})
		pod.Status = status
		Expect(k8sClient.Status().Update(context.TODO(), pod)).To(Succeed())
		return pod
	}
	runningAndReady := corev1.PodStatus{
		Phase:      corev1.PodRunning,
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
	}

	// nudge makes the operator look at the monitor now. Pods are not
	// watched; without it a change that only shows on a pod is noticed at
	// the next look, up to fifteen seconds later.
	nudge := func(key types.NamespacedName) {
		GinkgoHelper()
		nudges++
		obj := &pgpilotv1.PgpilotMonitor{}
		obj.Name, obj.Namespace = key.Name, key.Namespace
		patch := fmt.Appendf(nil, `{"metadata":{"annotations":{"test/nudge":"%d"}}}`, nudges)
		Expect(k8sClient.Patch(context.TODO(), obj, client.RawPatch(types.MergePatchType, patch))).To(Succeed())
	}

	monitorOf := func(g Gomega, key types.NamespacedName) *pgpilotv1.PgpilotMonitor {
		var m pgpilotv1.PgpilotMonitor
		g.Expect(k8sClient.Get(context.TODO(), key, &m)).To(Succeed())
		return &m
	}

	// expectReady waits for the Ready condition to take the given status and
	// reason, with status.ready and observedGeneration in line with it.
	expectReady := func(key types.NamespacedName, status metav1.ConditionStatus, reason string, messageParts ...string) {
		GinkgoHelper()
		Eventually(func(g Gomega) {
			m := monitorOf(g, key)
			cond := meta.FindStatusCondition(m.Status.Conditions, "Ready")
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Reason).To(Equal(reason), "message: %s", cond.Message)
			g.Expect(cond.Status).To(Equal(status))
			for _, part := range messageParts {
				g.Expect(cond.Message).To(ContainSubstring(part))
			}
			g.Expect(m.Status.Ready).To(Equal(status == metav1.ConditionTrue),
				"status.ready must say what the Ready condition says")
			g.Expect(m.Status.ObservedGeneration).To(Equal(m.Generation))
			g.Expect(cond.ObservedGeneration).To(Equal(m.Generation))
		}, timeout, interval).Should(Succeed())
	}

	eventsOf := func(key types.NamespacedName, reason string) []corev1.Event {
		GinkgoHelper()
		var list corev1.EventList
		Expect(k8sClient.List(context.TODO(), &list, client.InNamespace(key.Namespace))).To(Succeed())
		var out []corev1.Event
		for _, e := range list.Items {
			if e.InvolvedObject.Name == key.Name && e.Reason == reason {
				out = append(out, e)
			}
		}
		return out
	}

	// healthy creates a monitor and takes it to Ready=True.
	healthy := func(name string) types.NamespacedName {
		GinkgoHelper()
		monitor := minimalMonitor(ns, name)
		createForSpec(monitor)
		key := client.ObjectKeyFromObject(monitor)
		Eventually(func() error {
			return k8sClient.Get(context.TODO(), deploymentKey(key), &appsv1.Deployment{})
		}, timeout, interval).Should(Succeed())
		asDeploymentController(key, available)
		expectReady(key, metav1.ConditionTrue, "PodRunning")
		return key
	}

	setSpec := func(key types.NamespacedName, patch string) {
		GinkgoHelper()
		obj := &pgpilotv1.PgpilotMonitor{}
		obj.Name, obj.Namespace = key.Name, key.Namespace
		Expect(k8sClient.Patch(context.TODO(), obj, client.RawPatch(types.MergePatchType, []byte(patch)))).To(Succeed())
	}

	It("is not Ready before the collector exists, and says it is on its way", func() {
		monitor := minimalMonitor(ns, "fresh")
		createForSpec(monitor)
		key := client.ObjectKeyFromObject(monitor)

		// Nobody has acted on the Deployment yet.
		expectReady(key, metav1.ConditionFalse, "RolloutInProgress", "Deployment controller")
		// Nothing is wrong, so nothing is announced.
		Consistently(func() []corev1.Event {
			return append(eventsOf(key, "RolloutInProgress"), eventsOf(key, "PodNotReady")...)
		}, 2*time.Second, interval).Should(BeEmpty())

		asDeploymentController(key, appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, UnavailableReplicas: 1})
		expectReady(key, metav1.ConditionFalse, "PodNotReady")

		asDeploymentController(key, available)
		expectReady(key, metav1.ConditionTrue, "PodRunning", "pgwatch pod is running")
		Eventually(func() []corev1.Event { return eventsOf(key, "PodRunning") }, timeout, interval).Should(HaveLen(1))
	})

	// The pod of the previous spec keeps running, and stays ready, until the
	// pod of the new one is. One ready replica used to make the monitor
	// Ready=True at the new generation: a change that never got applied
	// looked like one that had.
	It("follows the rollout of a changed spec, not the pod that is still running", func() {
		key := healthy("rollout")
		old := collectorPod(key, "pgpilot-rollout-old", runningAndReady)
		nudge(key)
		Eventually(func(g Gomega) string { return monitorOf(g, key).Status.PodName }, timeout, interval).
			Should(Equal(old.Name))

		By("changing the spec; the Deployment still reports the old pod as its ready replica")
		setSpec(key, `{"spec":{"image":{"tag":"9.9.9"}}}`)
		expectReady(key, metav1.ConditionFalse, "RolloutInProgress")
		Eventually(func() []corev1.Event { return eventsOf(key, "RolloutInProgress") }, timeout, interval).
			Should(HaveLen(1), "a healthy monitor that starts rolling out says so, once")

		By("the new pod cannot pull its image")
		stuck := collectorPod(key, "pgpilot-rollout-new", corev1.PodStatus{
			Phase: corev1.PodPending,
			// As the kubelet reports a pod that is not ready: the condition
			// is there, and false.
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: "ContainersNotReady",
			}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "pgwatch", Image: "cybertecpostgresql/pgwatch:9.9.9",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "ImagePullBackOff", Message: `Back-off pulling image "cybertecpostgresql/pgwatch:9.9.9"`,
				}},
			}},
		})
		asDeploymentController(key, rollingOut)
		expectReady(key, metav1.ConditionFalse, "ImagePullBackOff",
			"Pod pgpilot-rollout-new cannot pull image", "cybertecpostgresql/pgwatch:9.9.9")
		Eventually(func() []corev1.Event { return eventsOf(key, "ImagePullBackOff") }, timeout, interval).
			Should(ConsistOf(HaveField("Type", corev1.EventTypeWarning)))
		Expect(monitorOf(Default, key).Status.PodName).To(Equal(old.Name), "the pod that runs is still the old one")

		By("the rollout runs out of time")
		Expect(k8sClient.Delete(context.TODO(), stuck, client.GracePeriodSeconds(0))).To(Succeed())
		timedOut := rollingOut
		timedOut.Conditions = []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded",
			Message: `ReplicaSet "pgpilot-rollout-5d4f" has timed out progressing.`,
		}}
		asDeploymentController(key, timedOut)
		expectReady(key, metav1.ConditionFalse, "RolloutStuck", "has timed out progressing")

		By("the image is fixed and the rollout completes")
		setSpec(key, `{"spec":{"image":{"tag":null}}}`)
		// The Deployment controller reports on the template it has seen:
		// wait for the corrected one before playing its part.
		Eventually(collectorImage(k8sClient, key), timeout, interval).Should(Equal(currentImage))
		asDeploymentController(key, available)
		expectReady(key, metav1.ConditionTrue, "PodRunning")
		// Announced a second time: the first was before the rollout. Events
		// with the same reason and message are counted on one object.
		Eventually(func() []corev1.Event { return eventsOf(key, "PodRunning") }, timeout, interval).
			Should(ConsistOf(HaveField("Count", BeNumerically(">=", 2))))
	})

	It("says why no pod could be created", func() {
		monitor := minimalMonitor(ns, "quota")
		createForSpec(monitor)
		key := client.ObjectKeyFromObject(monitor)
		Eventually(func() error {
			return k8sClient.Get(context.TODO(), deploymentKey(key), &appsv1.Deployment{})
		}, timeout, interval).Should(Succeed())

		asDeploymentController(key, appsv1.DeploymentStatus{
			UnavailableReplicas: 1,
			Conditions: []appsv1.DeploymentCondition{{
				Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue, Reason: "FailedCreate",
				Message: `pods "pgpilot-quota-7c9d-" is forbidden: exceeded quota: compute, requested: limits.memory=0`,
			}},
		})
		expectReady(key, metav1.ConditionFalse, "PodCreateFailed", "exceeded quota: compute")
		Eventually(func() []corev1.Event { return eventsOf(key, "PodCreateFailed") }, timeout, interval).
			Should(ConsistOf(HaveField("Type", corev1.EventTypeWarning)))
	})

	It("names the pod that runs and forgets it when it is gone", func() {
		key := healthy("pod-name")
		Expect(monitorOf(Default, key).Status.PodName).To(BeEmpty(), "no pod exists yet in this test")

		pod := collectorPod(key, "pgpilot-pod-name-abc", runningAndReady)
		nudge(key)
		Eventually(func(g Gomega) string { return monitorOf(g, key).Status.PodName }, timeout, interval).
			Should(Equal(pod.Name))

		Expect(k8sClient.Delete(context.TODO(), pod, client.GracePeriodSeconds(0))).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(context.TODO(), client.ObjectKeyFromObject(pod), &corev1.Pod{}))
		}, timeout, interval).Should(BeTrue())
		nudge(key)
		Eventually(func(g Gomega) string { return monitorOf(g, key).Status.PodName }, timeout, interval).
			Should(BeEmpty())
	})

	// A PgpilotMonitor may be called anything an object may be called; the
	// Service generated for it may not.
	DescribeTable("reports a name the generated objects cannot carry, and generates nothing",
		func(name string) {
			monitor := minimalMonitor(ns, name)
			createForSpec(monitor)
			key := client.ObjectKeyFromObject(monitor)

			expectReady(key, metav1.ConditionFalse, "InvalidName", "pgpilot-"+name, "at most 55 characters")
			Eventually(func() []corev1.Event { return eventsOf(key, "InvalidName") }, timeout, interval).
				Should(ConsistOf(HaveField("Type", corev1.EventTypeWarning)))

			Consistently(func(g Gomega) {
				for _, obj := range []client.Object{&appsv1.Deployment{}, &corev1.Service{}} {
					err := k8sClient.Get(context.TODO(), deploymentKey(key), obj)
					g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "%T exists: %v", obj, err)
				}
				var configMaps corev1.ConfigMapList
				g.Expect(k8sClient.List(context.TODO(), &configMaps, client.InNamespace(ns),
					client.MatchingLabels{builder.LabelMonitorName: name})).To(Succeed())
				g.Expect(configMaps.Items).To(BeEmpty())
			}, 2*time.Second, interval).Should(Succeed())
		},
		Entry("a dot", "db.prod"),
		Entry("56 characters", strings.Repeat("a", 56)),
	)

	It("runs a monitor whose name just fits", func() {
		monitor := minimalMonitor(ns, strings.Repeat("a", 55))
		createForSpec(monitor)
		key := client.ObjectKeyFromObject(monitor)
		Eventually(func() error {
			return k8sClient.Get(context.TODO(), deploymentKey(key), &corev1.Service{})
		}, timeout, interval).Should(Succeed())
	})

	// The CRD takes any resource requirements; the API server refuses a
	// Deployment whose requests exceed its limits. Sending it again at once
	// cannot help, so the monitor says what is wrong and the object is sent
	// again when the spec changes (and, in case the refusal came from
	// outside the spec, every five minutes).
	It("reports a generated object the API server refuses, without retrying it", func() {
		monitor := withSpec(minimalMonitor(ns, "rejected"), map[string]any{
			"requests": map[string]any{"memory": "2Gi"},
			"limits":   map[string]any{"memory": "1Gi"},
		}, "resources")
		createForSpec(monitor)
		key := client.ObjectKeyFromObject(monitor)

		expectReady(key, metav1.ConditionFalse, "ApplyRejected", "Deployment pgpilot-rejected", "limit")
		Eventually(func(g Gomega) {
			cond := meta.FindStatusCondition(monitorOf(g, key).Status.Conditions, "ConfigGenerated")
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue), "the ConfigMap was applied before the Deployment failed")
		}, timeout, interval).Should(Succeed())
		Eventually(func() []corev1.Event { return eventsOf(key, "ApplyRejected") }, timeout, interval).
			Should(ConsistOf(HaveField("Type", corev1.EventTypeWarning)))

		before := monitorReconciles()
		Consistently(func() []corev1.Event { return eventsOf(key, "ApplyRejected") }, 3*time.Second, interval).
			Should(ConsistOf(HaveField("Count", BeNumerically("<=", 1))))
		Expect(monitorReconciles()-before).To(BeNumerically("<=", 3), "a refused object must not be sent again and again")

		By("correcting the spec")
		setSpec(key, `{"spec":{"resources":{"limits":{"memory":"4Gi"}}}}`)
		Eventually(func() error {
			return k8sClient.Get(context.TODO(), deploymentKey(key), &appsv1.Deployment{})
		}, timeout, interval).Should(Succeed())
		expectReady(key, metav1.ConditionFalse, "RolloutInProgress")
	})

	// The names of the generated objects are predictable. An object that
	// happened to carry one was rewritten, given the monitor as its owner and
	// deleted with it. Such objects do not carry the operator's label, so
	// its cache does not even show them.
	It("leaves alone what already carries a generated name", func() {
		theirConfig := &corev1.ConfigMap{Data: map[string]string{"sources.yaml": "theirs"}}
		theirConfig.Name, theirConfig.Namespace = "pgpilot-taken-config", ns
		createForSpec(theirConfig)
		theirService := &corev1.Service{}
		theirService.Name, theirService.Namespace = "pgpilot-taken", ns
		theirService.Spec.Selector = map[string]string{"app": "theirs"}
		theirService.Spec.Ports = []corev1.ServicePort{{Port: 8080}}
		createForSpec(theirService)

		monitor := minimalMonitor(ns, "taken")
		createForSpec(monitor)
		key := client.ObjectKeyFromObject(monitor)

		expectReady(key, metav1.ConditionFalse, "NameConflict",
			"ConfigMap pgpilot-taken-config already exists", "it has no owner")
		Eventually(func() []corev1.Event { return eventsOf(key, "NameConflict") }, timeout, interval).
			Should(ConsistOf(HaveField("Type", corev1.EventTypeWarning)))
		Consistently(func(g Gomega) {
			var cm corev1.ConfigMap
			g.Expect(k8sClient.Get(context.TODO(), client.ObjectKeyFromObject(theirConfig), &cm)).To(Succeed())
			g.Expect(cm.Data).To(Equal(map[string]string{"sources.yaml": "theirs"}))
			g.Expect(cm.OwnerReferences).To(BeEmpty())
			g.Expect(cm.Labels).To(BeEmpty())
			err := k8sClient.Get(context.TODO(), deploymentKey(key), &appsv1.Deployment{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "a Deployment without its configuration: %v", err)
		}, 2*time.Second, interval).Should(Succeed())

		By("the ConfigMap is moved out of the way; the Service is still in it")
		Expect(k8sClient.Delete(context.TODO(), theirConfig)).To(Succeed())
		nudge(key)
		expectReady(key, metav1.ConditionFalse, "NameConflict", "Service pgpilot-taken already exists")
		Expect(k8sClient.Get(context.TODO(), deploymentKey(key), &appsv1.Deployment{})).To(Succeed())
		var svc corev1.Service
		Expect(k8sClient.Get(context.TODO(), client.ObjectKeyFromObject(theirService), &svc)).To(Succeed())
		Expect(svc.Spec.Selector).To(Equal(map[string]string{"app": "theirs"}))
		Expect(svc.OwnerReferences).To(BeEmpty())

		By("the Service is gone too")
		Expect(k8sClient.Delete(context.TODO(), theirService)).To(Succeed())
		nudge(key)
		expectReady(key, metav1.ConditionFalse, "RolloutInProgress")
		Expect(k8sClient.Get(context.TODO(), client.ObjectKeyFromObject(theirService), &svc)).To(Succeed())
		Expect(svc.OwnerReferences).To(ConsistOf(HaveField("Name", "taken")))
	})

	// A Service points at the port of the Prometheus sink. Without the sink
	// nothing listens there.
	It("has a Service only while the Prometheus sink is on", func() {
		monitor := minimalMonitor(ns, "sinks")
		createForSpec(monitor)
		key := client.ObjectKeyFromObject(monitor)
		serviceExists := func() bool {
			err := k8sClient.Get(context.TODO(), deploymentKey(key), &corev1.Service{})
			if err != nil {
				Expect(apierrors.IsNotFound(err)).To(BeTrue(), "%v", err)
			}
			return err == nil
		}
		Eventually(serviceExists, timeout, interval).Should(BeTrue())

		setSpec(key, `{"spec":{"sinks":{"prometheus":{"enabled":false},"grpc":{"enabled":true,"endpoint":"receiver.example.com:5000"}}}}`)
		Eventually(serviceExists, timeout, interval).Should(BeFalse())
		Expect(k8sClient.Get(context.TODO(), deploymentKey(key), &appsv1.Deployment{})).To(Succeed(), "the collector stays")

		setSpec(key, `{"spec":{"sinks":{"prometheus":{"enabled":true}}}}`)
		Eventually(serviceExists, timeout, interval).Should(BeTrue())
	})

	// Releases before 1.1 left status.ready out of a monitor that was not
	// ready, and the READY column empty. A status write that only carries
	// what changed would not put it there while the monitor stays not ready.
	It("writes status.ready into a monitor an earlier release left without it", func() {
		monitor := withSpec(minimalMonitor(ns, "no-ready-key"), "arrives-later", "database", "credentialsSecret", "name")
		createForSpec(monitor)
		key := client.ObjectKeyFromObject(monitor)
		expectReady(key, metav1.ConditionFalse, "SecretNotFound")

		readyKey := func(g Gomega) (value, present bool) {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(pgpilotv1.GroupVersion.WithKind("PgpilotMonitor"))
			g.Expect(k8sClient.Get(context.TODO(), key, obj)).To(Succeed())
			value, present, err := unstructured.NestedBool(obj.Object, "status", "ready")
			g.Expect(err).NotTo(HaveOccurred())
			return value, present
		}
		obj := &pgpilotv1.PgpilotMonitor{}
		obj.Name, obj.Namespace = key.Name, key.Namespace
		Expect(k8sClient.Status().Patch(context.TODO(), obj, client.RawPatch(types.JSONPatchType,
			[]byte(`[{"op":"remove","path":"/status/ready"}]`)))).To(Succeed())
		_, present := readyKey(Default)
		Expect(present).To(BeFalse(), "the field is gone, as those releases left it")

		By("the Secret arrives: not ready before, not ready now")
		secret := &corev1.Secret{StringData: map[string]string{"username": "pgwatch", "password": "s3cret"}}
		secret.Name, secret.Namespace = "arrives-later", ns
		createForSpec(secret)
		expectReady(key, metav1.ConditionFalse, "RolloutInProgress")
		value, present := readyKey(Default)
		Expect(present).To(BeTrue(), "status.ready is still missing")
		Expect(value).To(BeFalse())
	})

	// A monitor that waits for its Secret is looked at again and again.
	It("announces a missing Secret once, and writes nothing while it waits", func() {
		monitor := withSpec(minimalMonitor(ns, "waiting"), "not-there", "database", "credentialsSecret", "name")
		createForSpec(monitor)
		key := client.ObjectKeyFromObject(monitor)
		expectReady(key, metav1.ConditionFalse, "SecretNotFound", `Secret "not-there" not found`)

		var version string
		Eventually(func(g Gomega) {
			version = monitorOf(g, key).ResourceVersion
			g.Expect(eventsOf(key, "SecretNotFound")).To(HaveLen(1))
		}, timeout, interval).Should(Succeed())

		// The suite retries a missing Secret every second.
		before := monitorReconciles()
		Consistently(func(g Gomega) {
			g.Expect(monitorOf(g, key).ResourceVersion).To(Equal(version), "the status was written again")
			g.Expect(eventsOf(key, "SecretNotFound")).To(ConsistOf(HaveField("Count", BeNumerically("<=", 1))))
		}, 5*testCredentialsRetry, interval).Should(Succeed())
		Expect(monitorReconciles()-before).To(BeNumerically(">=", 3), "the monitor was not being retried in that time")

		cond := meta.FindStatusCondition(monitorOf(Default, key).Status.Conditions, "ConfigGenerated")
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionUnknown), "nothing was rendered, so nothing is known about it")
		Expect(cond.Reason).To(Equal("SecretNotFound"))
	})
})

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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
	"github.com/jamalshahverdiev/pgpilot-operator/internal/builder"
)

var _ = Describe("PgpilotMonitor credentials", func() {
	const (
		monitorName = "creds-db"
		secretName  = "db-creds"
	)
	var (
		ns         string
		monitorKey types.NamespacedName
		deployKey  types.NamespacedName
	)

	BeforeEach(func() {
		ns = testNamespace("test-creds-")
		monitorKey = types.NamespacedName{Name: monitorName, Namespace: ns}
		deployKey = types.NamespacedName{Name: "pgpilot-" + monitorName, Namespace: ns}
	})

	newMonitor := func(ref *pgpilotv1.CredentialsSecretRef) *pgpilotv1.PgpilotMonitor {
		m := &pgpilotv1.PgpilotMonitor{}
		m.Name, m.Namespace = monitorName, ns
		m.Spec.Database = pgpilotv1.DatabaseRef{
			Host: "pg.example.com", Database: "appdb", CredentialsSecret: ref,
		}
		m.Spec.Metrics = pgpilotv1.MetricsSpec{Preset: "basic"}
		m.Spec.Sinks = pgpilotv1.SinksSpec{Prometheus: &pgpilotv1.PrometheusSink{Enabled: true}}
		return m
	}
	newSecret := func(data map[string]string) *corev1.Secret {
		s := &corev1.Secret{StringData: data}
		s.Name, s.Namespace = secretName, ns
		return s
	}
	bySecret := &pgpilotv1.CredentialsSecretRef{Name: secretName}

	condition := func(g Gomega, conditionType string) *metav1.Condition {
		var m pgpilotv1.PgpilotMonitor
		g.Expect(k8sClient.Get(context.TODO(), monitorKey, &m)).To(Succeed())
		c := meta.FindStatusCondition(m.Status.Conditions, conditionType)
		g.Expect(c).NotTo(BeNil(), "condition %s is not set yet", conditionType)
		return c
	}
	expectUnavailable := func(reason, messagePart string) {
		GinkgoHelper()
		Eventually(func(g Gomega) {
			for _, conditionType := range []string{conditionCredentials, "Ready"} {
				c := condition(g, conditionType)
				g.Expect(c.Status).To(Equal(metav1.ConditionFalse), conditionType)
				g.Expect(c.Reason).To(Equal(reason), conditionType)
				g.Expect(c.Message).To(ContainSubstring(messagePart), conditionType)
			}
		}, timeout, interval).Should(Succeed())
	}
	expectAvailable := func(reason string) {
		GinkgoHelper()
		Eventually(func(g Gomega) {
			c := condition(g, conditionCredentials)
			g.Expect(c.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(c.Reason).To(Equal(reason))
		}, timeout, interval).Should(Succeed())
	}
	deployment := func(g Gomega) *appsv1.Deployment {
		var dep appsv1.Deployment
		g.Expect(k8sClient.Get(context.TODO(), deployKey, &dep)).To(Succeed())
		return &dep
	}
	expectNoDeployment := func() {
		GinkgoHelper()
		var dep appsv1.Deployment
		err := k8sClient.Get(context.TODO(), deployKey, &dep)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no collector may be started without credentials, got: %v", err)
	}
	credentialsVersionOf := func(g Gomega) string {
		return deployment(g).Spec.Template.Annotations[builder.AnnotationCredentialsVersion]
	}
	// becomeReady plays the part of the Deployment controller and the
	// kubelet, which envtest does not have: it reports the collector pod as
	// running. Without it a monitor never leaves the "not ready, look again
	// in 15 seconds" path, and the periodic recheck of the Secret - the only
	// thing that notices a rotation on a healthy monitor - is never reached.
	becomeReady := func() {
		GinkgoHelper()
		Eventually(func(g Gomega) {
			dep := deployment(g)
			dep.Status.ObservedGeneration = dep.Generation
			dep.Status.Replicas, dep.Status.UpdatedReplicas = 1, 1
			dep.Status.ReadyReplicas, dep.Status.AvailableReplicas = 1, 1
			g.Expect(k8sClient.Status().Update(context.TODO(), dep)).To(Succeed())
		}, timeout, interval).Should(Succeed())
		Eventually(func(g Gomega) {
			var m pgpilotv1.PgpilotMonitor
			g.Expect(k8sClient.Get(context.TODO(), monitorKey, &m)).To(Succeed())
			g.Expect(m.Status.Ready).To(BeTrue(), "the monitor must report the ready collector")
		}, timeout, interval).Should(Succeed())
	}
	lastReconciled := func(g Gomega) metav1.Time {
		var m pgpilotv1.PgpilotMonitor
		g.Expect(k8sClient.Get(context.TODO(), monitorKey, &m)).To(Succeed())
		g.Expect(m.Status.LastReconciled).NotTo(BeNil())
		return *m.Status.LastReconciled
	}
	// A recheck must be noticed well before the 15 second retry of a monitor
	// that is not ready could explain it.
	withinRechecks := 4 * testCredentialsRecheck
	Expect(withinRechecks).To(BeNumerically("<", 15*time.Second))

	// Nothing watches Secrets, so a Secret created after the monitor is only
	// found by looking again.
	It("waits for a Secret that does not exist yet and starts once it is created", func() {
		createForSpec(newMonitor(bySecret))
		expectUnavailable("SecretNotFound", secretName)
		expectNoDeployment()

		createForSpec(newSecret(map[string]string{"username": "pgwatch", "password": "s3cret"}))

		expectAvailable("CredentialsFound")
		Eventually(func(g Gomega) { deployment(g) }, timeout, interval).Should(Succeed())
	})

	// The pod would sit in CreateContainerConfigError while the condition
	// said the credentials were fine.
	DescribeTable("reports a Secret that cannot provide both values",
		func(ref *pgpilotv1.CredentialsSecretRef, data map[string]string, missingKey string) {
			ref.Name = secretName
			createForSpec(newSecret(data))
			createForSpec(newMonitor(ref))

			expectUnavailable("SecretKeyMissing", `"`+missingKey+`"`)
			expectNoDeployment()
		},
		Entry("no password", &pgpilotv1.CredentialsSecretRef{},
			map[string]string{"username": "pgwatch"}, "password"),
		Entry("no username", &pgpilotv1.CredentialsSecretRef{},
			map[string]string{"password": "s3cret"}, "username"),
		Entry("empty username", &pgpilotv1.CredentialsSecretRef{},
			map[string]string{"username": "", "password": "s3cret"}, "username"),
		Entry("values under other key names", &pgpilotv1.CredentialsSecretRef{},
			map[string]string{"user": "pgwatch", "pass": "s3cret"}, "username"),
		Entry("custom username key missing", &pgpilotv1.CredentialsSecretRef{UsernameKey: "db-user", PasswordKey: "db-pass"},
			map[string]string{"username": "pgwatch", "db-pass": "s3cret"}, "db-user"),
		Entry("custom password key missing", &pgpilotv1.CredentialsSecretRef{UsernameKey: "db-user", PasswordKey: "db-pass"},
			map[string]string{"db-user": "pgwatch", "password": "s3cret"}, "db-pass"),
	)

	// A database that trusts the monitoring role (trust or peer
	// authentication) has no password to give; such monitors ran before the
	// keys were checked and must keep running.
	It("accepts a password key that is present but empty", func() {
		createForSpec(newSecret(map[string]string{"username": "pgwatch", "password": ""}))
		createForSpec(newMonitor(bySecret))

		expectAvailable("CredentialsFound")
		Eventually(func(g Gomega) { deployment(g) }, timeout, interval).Should(Succeed())
	})

	It("accepts custom key names and wires them into the pod", func() {
		createForSpec(newSecret(map[string]string{"db-user": "pgwatch", "db-pass": "s3cret"}))
		createForSpec(newMonitor(&pgpilotv1.CredentialsSecretRef{
			Name: secretName, UsernameKey: "db-user", PasswordKey: "db-pass",
		}))

		expectAvailable("CredentialsFound")
		Eventually(func(g Gomega) {
			env := deployment(g).Spec.Template.Spec.Containers[0].Env
			keys := map[string]string{}
			for _, e := range env {
				g.Expect(e.ValueFrom).NotTo(BeNil(), "%s must come from the Secret", e.Name)
				g.Expect(e.ValueFrom.SecretKeyRef.Name).To(Equal(secretName))
				keys[e.Name] = e.ValueFrom.SecretKeyRef.Key
			}
			g.Expect(keys).To(Equal(map[string]string{"PGUSER": "db-user", "PGPASSWORD": "db-pass"}))
		}, timeout, interval).Should(Succeed())
	})

	It("starts the collector once a missing key is added", func() {
		secret := newSecret(map[string]string{"username": "pgwatch"})
		createForSpec(secret)
		createForSpec(newMonitor(bySecret))
		expectUnavailable("SecretKeyMissing", `"password"`)

		secret.StringData = map[string]string{"password": "s3cret"}
		Expect(k8sClient.Update(context.TODO(), secret)).To(Succeed())

		expectAvailable("CredentialsFound")
		Eventually(func(g Gomega) { deployment(g) }, timeout, interval).Should(Succeed())
	})

	// pgwatch reads PGUSER and PGPASSWORD once, when the container starts,
	// and keeps retrying with them when the database refuses the login. A
	// rotated password therefore needs a new pod.
	It("replaces the collector pod of a healthy monitor when the Secret changes", func() {
		secret := newSecret(map[string]string{"username": "pgwatch", "password": "old"})
		createForSpec(secret)
		createForSpec(newMonitor(bySecret))
		becomeReady()

		var before string
		var generation int64
		Eventually(func(g Gomega) {
			before = credentialsVersionOf(g)
			g.Expect(before).NotTo(BeEmpty(), "the pod template must carry the credentials version")
			generation = deployment(g).Generation
		}, timeout, interval).Should(Succeed())

		Expect(k8sClient.Get(context.TODO(), client.ObjectKeyFromObject(secret), secret)).To(Succeed())
		secret.StringData = map[string]string{"password": "rotated"}
		Expect(k8sClient.Update(context.TODO(), secret)).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(credentialsVersionOf(g)).NotTo(Equal(before))
			g.Expect(deployment(g).Generation).To(BeNumerically(">", generation),
				"the pod template must have changed for the pod to be replaced")
		}, withinRechecks, interval).Should(Succeed())
	})

	It("keeps rechecking an unchanged Secret without touching the collector pod", func() {
		createForSpec(newSecret(map[string]string{"username": "pgwatch", "password": "s3cret"}))
		createForSpec(newMonitor(bySecret))
		becomeReady()

		var version string
		var generation int64
		var reconciledAt metav1.Time
		Eventually(func(g Gomega) {
			version = credentialsVersionOf(g)
			g.Expect(version).NotTo(BeEmpty())
			generation = deployment(g).Generation
			reconciledAt = lastReconciled(g)
		}, timeout, interval).Should(Succeed())

		Consistently(func(g Gomega) {
			g.Expect(credentialsVersionOf(g)).To(Equal(version))
			g.Expect(deployment(g).Generation).To(Equal(generation))
		}, 3*testCredentialsRecheck, interval).Should(Succeed())

		// ... and rechecks did happen in that time; otherwise the above
		// would hold for a monitor nobody looks at any more.
		Eventually(func(g Gomega) {
			g.Expect(lastReconciled(g).After(reconciledAt.Time)).To(BeTrue(), "no recheck was observed")
		}, withinRechecks, interval).Should(Succeed())
	})

	It("never puts anything derived from the password onto the pod", func() {
		createForSpec(newSecret(map[string]string{"username": "pgwatch", "password": "hunter2"}))
		createForSpec(newMonitor(bySecret))

		var first string
		Eventually(func(g Gomega) {
			first = credentialsVersionOf(g)
			g.Expect(first).NotTo(BeEmpty())
		}, timeout, interval).Should(Succeed())

		// A second Secret with the very same values, in another namespace,
		// must give another version: the version identifies the Secret
		// object, it is not a fingerprint of the password.
		other := testNamespace("test-creds-other-")
		twin := newSecret(map[string]string{"username": "pgwatch", "password": "hunter2"})
		twin.Namespace = other
		createForSpec(twin)
		twinMonitor := newMonitor(bySecret)
		twinMonitor.Namespace = other
		createForSpec(twinMonitor)

		Eventually(func(g Gomega) {
			var dep appsv1.Deployment
			g.Expect(k8sClient.Get(context.TODO(),
				types.NamespacedName{Name: deployKey.Name, Namespace: other}, &dep)).To(Succeed())
			second := dep.Spec.Template.Annotations[builder.AnnotationCredentialsVersion]
			g.Expect(second).NotTo(BeEmpty())
			g.Expect(second).NotTo(Equal(first))
		}, timeout, interval).Should(Succeed())
	})

	It("reports a Secret that is deleted later", func() {
		secret := newSecret(map[string]string{"username": "pgwatch", "password": "s3cret"})
		createForSpec(secret)
		createForSpec(newMonitor(bySecret))
		expectAvailable("CredentialsFound")
		becomeReady()

		Expect(k8sClient.Delete(context.TODO(), secret)).To(Succeed())

		// Noticed by the recheck of the healthy monitor. The READY column
		// (status.ready) must not keep saying true next to Ready=False.
		Eventually(func(g Gomega) {
			c := condition(g, "Ready")
			g.Expect(c.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(c.Reason).To(Equal("SecretNotFound"))
			var m pgpilotv1.PgpilotMonitor
			g.Expect(k8sClient.Get(context.TODO(), monitorKey, &m)).To(Succeed())
			g.Expect(m.Status.Ready).To(BeFalse())
			g.Expect(m.Status.ObservedGeneration).To(Equal(m.Generation))
		}, withinRechecks, interval).Should(Succeed())
	})

	// The role grants create and patch on events; this is where it is used.
	It("records a Warning event on the monitor", func() {
		createForSpec(newMonitor(bySecret))
		expectUnavailable("SecretNotFound", secretName)

		Eventually(func(g Gomega) {
			var events corev1.EventList
			g.Expect(k8sClient.List(context.TODO(), &events, client.InNamespace(ns))).To(Succeed())
			var matching []string
			for _, e := range events.Items {
				if e.InvolvedObject.Kind == "PgpilotMonitor" && e.InvolvedObject.Name == monitorName &&
					e.Reason == "SecretNotFound" && e.Type == corev1.EventTypeWarning {
					matching = append(matching, e.Message)
				}
			}
			g.Expect(matching).NotTo(BeEmpty(), "no SecretNotFound event reached the API server")
			g.Expect(matching[0]).To(ContainSubstring(secretName))
		}, timeout, interval).Should(Succeed())
	})

	// Touching the monitor is the way to make the operator look at the
	// Secret at once instead of at the next recheck.
	It("reads the Secret again as soon as the monitor is annotated", func() {
		secret := newSecret(map[string]string{"username": "pgwatch", "password": "old"})
		createForSpec(secret)
		m := newMonitor(bySecret)
		createForSpec(m)

		var before string
		Eventually(func(g Gomega) {
			before = credentialsVersionOf(g)
			g.Expect(before).NotTo(BeEmpty())
		}, timeout, interval).Should(Succeed())

		Expect(k8sClient.Get(context.TODO(), client.ObjectKeyFromObject(secret), secret)).To(Succeed())
		secret.StringData = map[string]string{"password": "rotated"}
		Expect(k8sClient.Update(context.TODO(), secret)).To(Succeed())

		// The monitor is not ready (nothing reports the pod), so on its own
		// it would only come back after 15 seconds.
		Eventually(func(g Gomega) {
			var current pgpilotv1.PgpilotMonitor
			g.Expect(k8sClient.Get(context.TODO(), monitorKey, &current)).To(Succeed())
			patch := client.MergeFrom(current.DeepCopy())
			current.Annotations = map[string]string{"example.com/reconcile-requested": "now"}
			g.Expect(k8sClient.Patch(context.TODO(), &current, patch)).To(Succeed())
		}, timeout, interval).Should(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(credentialsVersionOf(g)).NotTo(Equal(before))
		}, 5*time.Second, interval).Should(Succeed())
	})

	It("marks inline credentials as available and adds no credentials version", func() {
		m := newMonitor(nil)
		m.Spec.Database.Username, m.Spec.Database.Password = "pgwatch", "s3cret"
		createForSpec(m)

		expectAvailable("InlineCredentials")
		Eventually(func(g Gomega) {
			g.Expect(deployment(g).Spec.Template.Annotations).NotTo(HaveKey(builder.AnnotationCredentialsVersion))
		}, timeout, interval).Should(Succeed())
	})

	// Nothing has to be read again for inline credentials, and the status
	// write that ends a reconcile must not start another one.
	It("does not keep reconciling a healthy monitor with inline credentials", func() {
		m := newMonitor(nil)
		m.Spec.Database.Username, m.Spec.Database.Password = "pgwatch", "s3cret"
		createForSpec(m)
		Eventually(func(g Gomega) { deployment(g) }, timeout, interval).Should(Succeed())
		becomeReady()

		var settled metav1.Time
		Eventually(func(g Gomega) {
			settled = lastReconciled(g)
			g.Expect(time.Since(settled.Time)).To(BeNumerically(">", 1500*time.Millisecond),
				"wait for the reconciles of the creation to end")
		}, timeout, interval).Should(Succeed())

		Consistently(func(g Gomega) {
			g.Expect(lastReconciled(g)).To(Equal(settled))
		}, 2*testCredentialsRecheck, interval).Should(Succeed())
	})

	// The condition used to be called DatabaseReachable although nothing
	// ever connected to the database.
	It("replaces the DatabaseReachable condition left by earlier releases", func() {
		createForSpec(newSecret(map[string]string{"username": "pgwatch", "password": "s3cret"}))
		createForSpec(newMonitor(bySecret))
		expectAvailable("CredentialsFound")

		Eventually(func(g Gomega) {
			var m pgpilotv1.PgpilotMonitor
			g.Expect(k8sClient.Get(context.TODO(), monitorKey, &m)).To(Succeed())
			meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
				Type: legacyConditionDatabaseReachable, Status: metav1.ConditionTrue,
				Reason: "CredentialsFound", Message: "left behind by an earlier release",
			})
			g.Expect(k8sClient.Status().Update(context.TODO(), &m)).To(Succeed())
		}, timeout, interval).Should(Succeed())

		Eventually(func(g Gomega) {
			var m pgpilotv1.PgpilotMonitor
			g.Expect(k8sClient.Get(context.TODO(), monitorKey, &m)).To(Succeed())
			g.Expect(meta.FindStatusCondition(m.Status.Conditions, legacyConditionDatabaseReachable)).To(BeNil())
			g.Expect(meta.FindStatusCondition(m.Status.Conditions, conditionCredentials)).NotTo(BeNil())
		}, timeout, interval).Should(Succeed())
	})
})

// The operator runs with the ClusterRole of config/rbac/role.yaml (see the
// suite setup) and the production cache options. These specs pin what that
// means for the data it can reach and the data it keeps.
var _ = Describe("Operator access", func() {
	It("can read a Secret by name but cannot list or watch Secrets", func() {
		ns := testNamespace("test-access-")
		secret := &corev1.Secret{StringData: map[string]string{"username": "u"}}
		secret.Name, secret.Namespace = "one", ns
		createForSpec(secret)

		asOperator, err := client.New(operatorConfig, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())

		var got corev1.Secret
		Expect(asOperator.Get(context.TODO(), client.ObjectKeyFromObject(secret), &got)).To(Succeed())

		var list corev1.SecretList
		err = asOperator.List(context.TODO(), &list, client.InNamespace(ns))
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "listing Secrets must be forbidden, got: %v", err)
		err = asOperator.List(context.TODO(), &list)
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "listing Secrets cluster-wide must be forbidden, got: %v", err)

		watching, err := client.NewWithWatch(operatorConfig, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())
		_, err = watching.Watch(context.TODO(), &list, client.InNamespace(ns))
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "watching Secrets must be forbidden, got: %v", err)

		for name, change := range map[string]func() error{
			"create": func() error {
				s := &corev1.Secret{}
				s.Name, s.Namespace = "written-by-operator", ns
				return asOperator.Create(context.TODO(), s)
			},
			"update": func() error { return asOperator.Update(context.TODO(), got.DeepCopy()) },
			"delete": func() error { return asOperator.Delete(context.TODO(), got.DeepCopy()) },
		} {
			Expect(apierrors.IsForbidden(change())).To(BeTrue(), "%s of a Secret must be forbidden", name)
		}
	})

	// Unfiltered informers held every ConfigMap, Deployment, Service and Pod
	// of the cluster in the operator's memory.
	It("keeps only the operator's own objects in its cache", func() {
		ns := testNamespace("test-cache-")

		foreign := &corev1.ConfigMap{Data: map[string]string{"big": "not ours"}}
		foreign.Name, foreign.Namespace = "someone-elses", ns
		createForSpec(foreign)

		ours := &corev1.ConfigMap{}
		ours.Name, ours.Namespace = "pgpilot-example-config", ns
		ours.Labels = builder.SystemLabels("example")
		createForSpec(ours)

		Eventually(func(g Gomega) {
			var cached corev1.ConfigMapList
			g.Expect(managerCache.List(context.TODO(), &cached, client.InNamespace(ns))).To(Succeed())
			names := make([]string, 0, len(cached.Items))
			for _, cm := range cached.Items {
				names = append(names, cm.Name)
			}
			g.Expect(names).To(ConsistOf("pgpilot-example-config"))
		}, timeout, interval).Should(Succeed())

		var all corev1.ConfigMapList
		Expect(managerCache.List(context.TODO(), &all)).To(Succeed())
		for _, cm := range all.Items {
			Expect(cm.Labels).To(HaveKeyWithValue(builder.LabelAppManagedBy, builder.ManagedBy),
				"ConfigMap %s/%s is cached although the operator does not manage it", cm.Namespace, cm.Name)
			Expect(cm.ManagedFields).To(BeEmpty(), "managedFields must be stripped from cached objects")
		}
	})

	It("applies the same filter to Deployments and Services", func() {
		ns := testNamespace("test-cache-kinds-")
		labelled := builder.SystemLabels("example")

		deployment := func(name string, labels map[string]string) *appsv1.Deployment {
			d := &appsv1.Deployment{}
			d.Name, d.Namespace, d.Labels = name, ns, labels
			d.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}}
			d.Spec.Template.Labels = map[string]string{"app": name}
			d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "c", Image: "example.com/image:1"}}
			return d
		}
		service := func(name string, labels map[string]string) *corev1.Service {
			svc := &corev1.Service{}
			svc.Name, svc.Namespace, svc.Labels = name, ns, labels
			svc.Spec.Ports = []corev1.ServicePort{{Port: 80}}
			return svc
		}
		createForSpec(deployment("foreign", nil))
		createForSpec(deployment("ours", labelled))
		createForSpec(service("foreign", nil))
		createForSpec(service("ours", labelled))

		Eventually(func(g Gomega) {
			var deployments appsv1.DeploymentList
			g.Expect(managerCache.List(context.TODO(), &deployments, client.InNamespace(ns))).To(Succeed())
			g.Expect(deployments.Items).To(HaveLen(1))
			g.Expect(deployments.Items[0].Name).To(Equal("ours"))

			var services corev1.ServiceList
			g.Expect(managerCache.List(context.TODO(), &services, client.InNamespace(ns))).To(Succeed())
			g.Expect(services.Items).To(HaveLen(1))
			g.Expect(services.Items[0].Name).To(Equal("ours"))
		}, timeout, interval).Should(Succeed())
	})

	It("finds the pods of its Deployments through the cache", func() {
		ns := testNamespace("test-cache-pods-")
		pod := func(name string, labels map[string]string) *corev1.Pod {
			p := &corev1.Pod{}
			p.Name, p.Namespace, p.Labels = name, ns, labels
			p.Spec.Containers = []corev1.Container{{Name: "c", Image: "example.com/image:1"}}
			return p
		}
		createForSpec(pod("unrelated", map[string]string{"app": "other"}))
		createForSpec(pod("pgpilot-example-abc", builder.SystemLabels("example")))

		Eventually(func(g Gomega) {
			var cached corev1.PodList
			g.Expect(managerCache.List(context.TODO(), &cached, client.InNamespace(ns))).To(Succeed())
			names := make([]string, 0, len(cached.Items))
			for _, p := range cached.Items {
				names = append(names, p.Name)
			}
			g.Expect(names).To(ConsistOf("pgpilot-example-abc"))
		}, timeout, interval).Should(Succeed())
	})
})

// unitMonitor returns a monitor for the fake-client tests below.
func unitMonitor() *pgpilotv1.PgpilotMonitor {
	monitor := &pgpilotv1.PgpilotMonitor{}
	monitor.Name, monitor.Namespace, monitor.Generation = "db", "team", 3
	monitor.Spec.Database = pgpilotv1.DatabaseRef{
		Host: "pg.example.com", Database: "appdb",
		CredentialsSecret: &pgpilotv1.CredentialsSecretRef{Name: "db-creds"},
	}
	monitor.Spec.Metrics = pgpilotv1.MetricsSpec{Preset: "basic"}
	monitor.Spec.Sinks = pgpilotv1.SinksSpec{Prometheus: &pgpilotv1.PrometheusSink{Enabled: true}}
	return monitor
}

func unitScheme(t *testing.T) {
	t.Helper()
	if err := pgpilotv1.AddToScheme(scheme.Scheme); err != nil {
		t.Fatal(err)
	}
}

// A Secret that cannot be read at all - the API server is unreachable, or
// refuses - is not a missing Secret. It must be retried with backoff and
// counted as a failed reconcile, not reported as SecretNotFound.
func TestReconcile_SecretReadFailureIsAnErrorNotAMissingSecret(t *testing.T) {
	unitScheme(t)
	secrets := schema.GroupResource{Resource: "secrets"}
	failures := map[string]error{
		"connection problem":  errors.New("etcdserver: request timed out"),
		"forbidden":           apierrors.NewForbidden(secrets, "db-creds", errors.New("RBAC: denied")),
		"service unavailable": apierrors.NewServiceUnavailable("apiserver is shutting down"),
		"too many requests":   apierrors.NewTooManyRequests("slow down", 1),
	}
	for name, failure := range failures {
		t.Run(name, func(t *testing.T) {
			monitor := unitMonitor()
			// As left by an earlier, successful reconcile.
			monitor.Status.Ready = true
			c := fake.NewClientBuilder().
				WithScheme(scheme.Scheme).
				WithObjects(monitor).
				WithStatusSubresource(monitor).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey,
						obj client.Object, opts ...client.GetOption) error {
						if _, isSecret := obj.(*corev1.Secret); isSecret {
							return failure
						}
						return cl.Get(ctx, key, obj, opts...)
					},
				}).
				Build()

			recorder := record.NewFakeRecorder(10)
			r := &PgpilotMonitorReconciler{Client: c, Scheme: scheme.Scheme, Recorder: recorder, CredentialsRetry: time.Hour}

			result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(monitor)})
			if !errors.Is(err, failure) {
				t.Fatalf("Reconcile error = %v, want it to wrap the read failure", err)
			}
			if result.RequeueAfter != 0 {
				t.Errorf("RequeueAfter = %v: a returned error is retried by the work queue, not on a timer", result.RequeueAfter)
			}

			var got pgpilotv1.PgpilotMonitor
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(monitor), &got); err != nil {
				t.Fatal(err)
			}
			for _, conditionType := range []string{conditionCredentials, "Ready"} {
				cond := meta.FindStatusCondition(got.Status.Conditions, conditionType)
				if cond == nil {
					t.Fatalf("condition %s was not written", conditionType)
				}
				if cond.Status != metav1.ConditionFalse || cond.Reason != "SecretReadFailed" {
					t.Errorf("%s = %s/%s, want False/SecretReadFailed", conditionType, cond.Status, cond.Reason)
				}
			}
			if got.Status.Ready {
				t.Error("status.ready is still true next to Ready=False")
			}
			if got.Status.ObservedGeneration != monitor.Generation {
				t.Errorf("observedGeneration = %d, want %d", got.Status.ObservedGeneration, monitor.Generation)
			}

			select {
			case event := <-recorder.Events:
				if !strings.HasPrefix(event, "Warning SecretReadFailed") {
					t.Errorf("event = %q, want a SecretReadFailed warning", event)
				}
			default:
				t.Error("no event was recorded")
			}

			var deployments appsv1.DeploymentList
			if err := c.List(t.Context(), &deployments); err != nil {
				t.Fatal(err)
			}
			if len(deployments.Items) != 0 {
				t.Errorf("a Deployment was created although the credentials could not be read")
			}
		})
	}
}

// States the user has to change are reported and looked at again after the
// retry interval; they are not reconcile errors.
func TestReconcile_UnusableCredentialsAreReportedAndRetried(t *testing.T) {
	unitScheme(t)
	secretWith := func(data map[string]string) *corev1.Secret {
		s := &corev1.Secret{Data: map[string][]byte{}}
		s.Name, s.Namespace = "db-creds", "team"
		for k, v := range data {
			s.Data[k] = []byte(v)
		}
		return s
	}
	tests := []struct {
		name       string
		mutate     func(*pgpilotv1.PgpilotMonitor)
		secret     *corev1.Secret
		wantReason string
	}{
		{name: "no Secret", wantReason: "SecretNotFound"},
		{name: "no password key", secret: secretWith(map[string]string{"username": "u"}), wantReason: "SecretKeyMissing"},
		{name: "empty username", secret: secretWith(map[string]string{"username": "", "password": "p"}), wantReason: "SecretKeyMissing"},
		{
			// The CRD does not admit this; an object stored before the
			// rule existed can still look like it.
			name:       "neither inline credentials nor a Secret",
			mutate:     func(m *pgpilotv1.PgpilotMonitor) { m.Spec.Database.CredentialsSecret = nil },
			wantReason: "NoCredentials",
		},
		{
			name: "inline username without a password",
			mutate: func(m *pgpilotv1.PgpilotMonitor) {
				m.Spec.Database.CredentialsSecret = nil
				m.Spec.Database.Username = "pgwatch"
			},
			wantReason: "NoCredentials",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			monitor := unitMonitor()
			monitor.Status.Ready = true
			meta.SetStatusCondition(&monitor.Status.Conditions, metav1.Condition{
				Type: legacyConditionDatabaseReachable, Status: metav1.ConditionTrue, Reason: "CredentialsFound",
			})
			if tt.mutate != nil {
				tt.mutate(monitor)
			}
			objects := []client.Object{monitor}
			if tt.secret != nil {
				objects = append(objects, tt.secret)
			}
			c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objects...).
				WithStatusSubresource(monitor).Build()
			recorder := record.NewFakeRecorder(10)
			r := &PgpilotMonitorReconciler{Client: c, Scheme: scheme.Scheme, Recorder: recorder, CredentialsRetry: 7 * time.Second}

			result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(monitor)})
			if err != nil {
				t.Fatalf("Reconcile returned an error for a state the user has to fix: %v", err)
			}
			if result.RequeueAfter != 7*time.Second {
				t.Errorf("RequeueAfter = %v, want the retry interval of 7s", result.RequeueAfter)
			}

			var got pgpilotv1.PgpilotMonitor
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(monitor), &got); err != nil {
				t.Fatal(err)
			}
			for _, conditionType := range []string{conditionCredentials, "Ready"} {
				cond := meta.FindStatusCondition(got.Status.Conditions, conditionType)
				if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != tt.wantReason {
					t.Errorf("%s = %+v, want False/%s", conditionType, cond, tt.wantReason)
				}
			}
			if meta.FindStatusCondition(got.Status.Conditions, legacyConditionDatabaseReachable) != nil {
				t.Error("the DatabaseReachable condition of earlier releases was not removed")
			}
			if got.Status.Ready {
				t.Error("status.ready is still true next to Ready=False")
			}
			select {
			case event := <-recorder.Events:
				if !strings.HasPrefix(event, "Warning "+tt.wantReason) {
					t.Errorf("event = %q, want a %s warning", event, tt.wantReason)
				}
			default:
				t.Error("no event was recorded")
			}
		})
	}
}

func TestCredentialsVersion(t *testing.T) {
	secret := func(uid, resourceVersion string, data map[string]string) *corev1.Secret {
		s := &corev1.Secret{Data: map[string][]byte{}}
		s.UID, s.ResourceVersion = types.UID(uid), resourceVersion
		for k, v := range data {
			s.Data[k] = []byte(v)
		}
		return s
	}
	base := credentialsVersion(secret("uid-1", "100", map[string]string{"password": "a"}))

	if len(base) != 16 {
		t.Errorf("version %q: want 16 hex characters", base)
	}
	if got := credentialsVersion(secret("uid-1", "100", map[string]string{"password": "a"})); got != base {
		t.Errorf("same Secret gave %q and %q", base, got)
	}
	if got := credentialsVersion(secret("uid-1", "101", map[string]string{"password": "a"})); got == base {
		t.Error("an updated Secret must give a new version")
	}
	if got := credentialsVersion(secret("uid-2", "100", map[string]string{"password": "a"})); got == base {
		t.Error("a recreated Secret must give a new version")
	}
	// Not a fingerprint of the content: a different password on the same
	// object state cannot be told apart, and so cannot be guessed from it.
	if got := credentialsVersion(secret("uid-1", "100", map[string]string{"password": "b"})); got != base {
		t.Error("the version must not depend on the Secret's values")
	}
}

func TestReconcilerIntervals_DefaultWhenUnset(t *testing.T) {
	r := &PgpilotMonitorReconciler{}
	if got := r.credentialsRecheck(); got != 5*time.Minute {
		t.Errorf("default recheck = %v, want 5m", got)
	}
	if got := r.credentialsRetry(); got != 30*time.Second {
		t.Errorf("default retry = %v, want 30s", got)
	}
	r = &PgpilotMonitorReconciler{CredentialsRecheck: time.Minute, CredentialsRetry: 5 * time.Second}
	if r.credentialsRecheck() != time.Minute || r.credentialsRetry() != 5*time.Second {
		t.Errorf("configured intervals are not used: %v, %v", r.credentialsRecheck(), r.credentialsRetry())
	}
}

// A preset that cannot be collected is a mistake in the spec. Retrying it
// changes nothing, and a returned error would have the controller retry it
// with backoff for ever; the next edit of the spec starts a reconcile anyway.
func TestReconcile_InvalidPresetIsReportedAndNotRetried(t *testing.T) {
	unitScheme(t)
	for _, preset := range []string{"exhaustiv", "patroni", "pgbouncer"} {
		t.Run(preset, func(t *testing.T) {
			monitor := unitMonitor()
			monitor.Spec.Metrics.Preset = preset
			secret := &corev1.Secret{Data: map[string][]byte{"username": []byte("u"), "password": []byte("p")}}
			secret.Name, secret.Namespace = "db-creds", "team"
			c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
				WithObjects(monitor, secret).WithStatusSubresource(monitor).Build()
			recorder := record.NewFakeRecorder(10)
			r := &PgpilotMonitorReconciler{Client: c, Scheme: scheme.Scheme, Recorder: recorder}

			result, err := r.Reconcile(context.Background(),
				ctrl.Request{NamespacedName: client.ObjectKeyFromObject(monitor)})
			if err != nil {
				t.Errorf("Reconcile returned %v: an error is retried with backoff for ever", err)
			}
			if result != (ctrl.Result{}) {
				t.Errorf("Reconcile asked for another run (%+v); nothing can change until the spec does", result)
			}

			var got pgpilotv1.PgpilotMonitor
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(monitor), &got); err != nil {
				t.Fatal(err)
			}
			if got.Status.Ready || got.Status.ObservedGeneration != monitor.Generation {
				t.Errorf("status: ready=%v observedGeneration=%d", got.Status.Ready, got.Status.ObservedGeneration)
			}
			for _, conditionType := range []string{"Ready", "ConfigGenerated"} {
				cond := meta.FindStatusCondition(got.Status.Conditions, conditionType)
				if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "InvalidPreset" ||
					!strings.Contains(cond.Message, strconv.Quote(preset)) {
					t.Errorf("condition %s: %+v", conditionType, cond)
				}
			}
			// The credentials were fine, and say so.
			if cond := meta.FindStatusCondition(got.Status.Conditions, "CredentialsAvailable"); cond == nil ||
				cond.Status != metav1.ConditionTrue {
				t.Errorf("condition CredentialsAvailable: %+v", cond)
			}
			events := drain(recorder)
			if len(events) != 1 || !strings.HasPrefix(events[0], "Warning InvalidPreset ") {
				t.Errorf("events: %q", events)
			}

			var deployments appsv1.DeploymentList
			var configMaps corev1.ConfigMapList
			if err := c.List(context.Background(), &deployments); err != nil {
				t.Fatal(err)
			}
			if err := c.List(context.Background(), &configMaps); err != nil {
				t.Fatal(err)
			}
			if len(deployments.Items)+len(configMaps.Items) != 0 {
				t.Errorf("generated %d Deployments and %d ConfigMaps for a preset that collects nothing",
					len(deployments.Items), len(configMaps.Items))
			}
		})
	}
}

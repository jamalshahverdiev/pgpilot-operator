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
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
	"github.com/jamalshahverdiev/pgpilot-operator/internal/builder"
)

const (
	timeout  = 30 * time.Second
	interval = 250 * time.Millisecond
)

var _ = Describe("PgpilotMonitor Controller", func() {
	var (
		ns      *corev1.Namespace
		secret  *corev1.Secret
		monitor *pgpilotv1.PgpilotMonitor
	)

	BeforeEach(func() {
		ns = &corev1.Namespace{
			GenerateName: "test-ns-",
		}
		Expect(k8sClient.Create(context.TODO(), ns)).To(Succeed())

		secret = &corev1.Secret{
			Name:      "db-creds",
			Namespace: ns.Name,
			StringData: map[string]string{
				"username": "pgwatch",
				"password": "secret",
			},
		}
		createForSpec(secret)

		monitor = &pgpilotv1.PgpilotMonitor{
			Name:      "test-db",
			Namespace: ns.Name,
			Spec: pgpilotv1.PgpilotMonitorSpec{
				Database: pgpilotv1.DatabaseRef{
					Host:     "pg.example.com",
					Port:     5432,
					Database: "testdb",
					SSLMode:  pgpilotv1.SSLModeDisable,
					CredentialsSecret: &pgpilotv1.CredentialsSecretRef{
						Name: "db-creds",
					},
				},
				Metrics: pgpilotv1.MetricsSpec{
					Preset: "basic",
				},
				Sinks: pgpilotv1.SinksSpec{
					Prometheus: &pgpilotv1.PrometheusSink{
						Enabled: true,
						Port:    9187,
					},
				},
			},
		}
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(context.TODO(), ns)).To(Succeed())
	})

	Context("with valid Secret", func() {
		const webDisableArg = "--web-disable=all"

		pgwatchArgs := func(g Gomega) (*appsv1.Deployment, []string) {
			var dep appsv1.Deployment
			g.Expect(k8sClient.Get(context.TODO(),
				types.NamespacedName{Name: "pgpilot-test-db", Namespace: ns.Name}, &dep)).To(Succeed())
			g.Expect(dep.Spec.Template.Spec.Containers).To(HaveLen(1))
			return &dep, dep.Spec.Template.Spec.Containers[0].Args
		}

		It("should start pgwatch with its web server disabled", func() {
			createForSpec(monitor)

			Eventually(func(g Gomega) {
				_, args := pgwatchArgs(g)
				g.Expect(args).To(ContainElement(webDisableArg))
			}, timeout, interval).Should(Succeed())
		})

		// Deployments created by an operator release that did not pass the
		// flag must be corrected in place, which is what rolls the collector
		// pods when the operator is upgraded.
		It("should add the web server flag to an existing Deployment that lacks it", func() {
			createForSpec(monitor)

			var legacy *appsv1.Deployment
			Eventually(func(g Gomega) {
				dep, args := pgwatchArgs(g)
				g.Expect(args).To(ContainElement(webDisableArg))
				legacy = dep.DeepCopy()
			}, timeout, interval).Should(Succeed())

			// Put the Deployment back into the shape the previous release
			// left it in, using the operator's own field manager.
			legacy.TypeMeta = metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}
			legacy.ManagedFields = nil
			legacy.ResourceVersion = ""
			legacy.Status = appsv1.DeploymentStatus{}
			container := &legacy.Spec.Template.Spec.Containers[0]
			container.Args = slices.DeleteFunc(container.Args, func(a string) bool { return a == webDisableArg })
			Expect(container.Args).NotTo(ContainElement(webDisableArg))

			//nolint:staticcheck // SA1019: same apply call the reconciler uses
			Expect(k8sClient.Patch(context.TODO(), legacy, client.Apply,
				client.FieldOwner("pgpilot-operator"), client.ForceOwnership)).To(Succeed())
			legacyGeneration := legacy.Generation

			Eventually(func(g Gomega) {
				dep, args := pgwatchArgs(g)
				g.Expect(args).To(ContainElement(webDisableArg))
				g.Expect(dep.Generation).To(BeNumerically(">", legacyGeneration),
					"the pod template must change so the collector pod is replaced")
			}, timeout, interval).Should(Succeed())
		})

		It("should create ConfigMap, Deployment, and Service", func() {
			createForSpec(monitor)

			monitorKey := types.NamespacedName{Name: "test-db", Namespace: ns.Name}
			Eventually(func() error {
				_, err := mountedConfigMap(k8sClient, monitorKey)
				return err
			}, timeout, interval).Should(Succeed())

			depKey := types.NamespacedName{
				Name:      builder.ResourceName("test-db"),
				Namespace: ns.Name,
			}
			Eventually(func() error {
				return k8sClient.Get(context.TODO(), depKey, &appsv1.Deployment{})
			}, timeout, interval).Should(Succeed())

			svcKey := types.NamespacedName{
				Name:      builder.ResourceName("test-db"),
				Namespace: ns.Name,
			}
			Eventually(func() error {
				return k8sClient.Get(context.TODO(), svcKey, &corev1.Service{})
			}, timeout, interval).Should(Succeed())
		})

		It("should set ConfigGenerated condition to True", func() {
			createForSpec(monitor)

			Eventually(func() bool {
				var m pgpilotv1.PgpilotMonitor
				if err := k8sClient.Get(context.TODO(), types.NamespacedName{Name: "test-db", Namespace: ns.Name}, &m); err != nil {
					return false
				}
				for _, c := range m.Status.Conditions {
					if c.Type == "ConfigGenerated" && c.Status == metav1.ConditionTrue {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())
		})

		It("should set CredentialsAvailable condition to True", func() {
			createForSpec(monitor)

			Eventually(func() bool {
				var m pgpilotv1.PgpilotMonitor
				if err := k8sClient.Get(context.TODO(), types.NamespacedName{Name: "test-db", Namespace: ns.Name}, &m); err != nil {
					return false
				}
				for _, c := range m.Status.Conditions {
					if c.Type == conditionCredentials && c.Status == metav1.ConditionTrue {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())
		})

		// The operator used to put a finalizer on every monitor, with a full
		// Update of the object: the spec went back as the Go struct
		// serialises it, the API server filled the empty `image` block with
		// defaults, and the operator ended up owning fields the user never
		// wrote. The finalizer protected nothing and made every deletion
		// wait for a running operator.
		It("should leave the monitor as the user wrote it: no finalizer, no rewritten spec", func() {
			// Created the way kubectl sends a manifest: only the fields the
			// user wrote. (The typed monitor of this suite would itself
			// send empty `image`, `resources` and metadata blocks.)
			written := minimalMonitor(ns.Name, "as-written")
			createForSpec(written)

			stored := written.DeepCopy()
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(context.TODO(), client.ObjectKeyFromObject(written), stored)).To(Succeed())
				_, hasStatus, _ := unstructured.NestedMap(stored.Object, "status")
				g.Expect(hasStatus).To(BeTrue(), "wait for a full reconcile")
			}, timeout, interval).Should(Succeed())

			Consistently(func(g Gomega) {
				g.Expect(k8sClient.Get(context.TODO(), client.ObjectKeyFromObject(written), stored)).To(Succeed())
				g.Expect(stored.GetFinalizers()).To(BeEmpty())
				g.Expect(stored.GetGeneration()).To(BeEquivalentTo(1), "the spec must not have been written")
			}, 2*time.Second, interval).Should(Succeed())
			for _, field := range []string{"image", "resources", "podMetadata", "serviceMetadata"} {
				_, found, err := unstructured.NestedFieldNoCopy(stored.Object, "spec", field)
				Expect(err).NotTo(HaveOccurred())
				Expect(found).To(BeFalse(), "spec.%s was never set by the user and must stay unset", field)
			}
		})

		// Nothing of the operator's stands between a delete and its end: the
		// request itself removes the object, whether the operator runs or
		// not. With a finalizer the object would still be there, waiting.
		It("should be gone as soon as it is deleted", func() {
			createForSpec(monitor)
			key := types.NamespacedName{Name: "test-db", Namespace: ns.Name}
			Eventually(func(g Gomega) {
				var m pgpilotv1.PgpilotMonitor
				g.Expect(k8sClient.Get(context.TODO(), key, &m)).To(Succeed())
				g.Expect(m.Status.ObservedGeneration).To(Equal(m.Generation), "wait for a full reconcile")
			}, timeout, interval).Should(Succeed())

			Expect(k8sClient.Delete(context.TODO(), monitor)).To(Succeed())
			var m pgpilotv1.PgpilotMonitor
			err := k8sClient.Get(context.TODO(), key, &m)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the monitor is still there after its deletion: %v", err)
		})

		It("should let go of a monitor that still carries the finalizer of an earlier release", func() {
			monitor.Finalizers = []string{finalizerName}
			createForSpec(monitor)
			key := types.NamespacedName{Name: "test-db", Namespace: ns.Name}

			Expect(k8sClient.Delete(context.TODO(), monitor)).To(Succeed())

			Eventually(func() bool {
				var m pgpilotv1.PgpilotMonitor
				return apierrors.IsNotFound(k8sClient.Get(context.TODO(), key, &m))
			}, timeout, interval).Should(BeTrue(), "the monitor must be gone once its finalizer is released")
		})

		It("should set OwnerReference on child resources", func() {
			createForSpec(monitor)

			monitorKey := types.NamespacedName{Name: "test-db", Namespace: ns.Name}
			Eventually(func() bool {
				cm, err := mountedConfigMap(k8sClient, monitorKey)
				if err != nil {
					return false
				}
				for _, ref := range cm.OwnerReferences {
					if ref.Kind == "PgpilotMonitor" && ref.Name == "test-db" {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())
		})
	})

	Context("with missing Secret", func() {
		It("should set Ready=False with SecretNotFound reason", func() {
			monitor.Spec.Database.CredentialsSecret.Name = "nonexistent-secret"
			createForSpec(monitor)

			Eventually(func() string {
				var m pgpilotv1.PgpilotMonitor
				if err := k8sClient.Get(context.TODO(), types.NamespacedName{Name: "test-db", Namespace: ns.Name}, &m); err != nil {
					return ""
				}
				for _, c := range m.Status.Conditions {
					if c.Type == "Ready" && c.Status == metav1.ConditionFalse {
						return c.Reason
					}
				}
				return ""
			}, timeout, interval).Should(Equal("SecretNotFound"))
		})
	})
})

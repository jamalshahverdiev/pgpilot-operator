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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	pgpilotv1 "github.com/jamalshahverdiyev/pgpilot-operator/api/v1"
	"github.com/jamalshahverdiyev/pgpilot-operator/internal/builder"
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
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-ns-",
			},
		}
		Expect(k8sClient.Create(context.TODO(), ns)).To(Succeed())

		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "db-creds",
				Namespace: ns.Name,
			},
			StringData: map[string]string{
				"username": "pgwatch",
				"password": "secret",
			},
		}
		Expect(k8sClient.Create(context.TODO(), secret)).To(Succeed())

		monitor = &pgpilotv1.PgpilotMonitor{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-db",
				Namespace: ns.Name,
			},
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
		It("should create ConfigMap, Deployment, and Service", func() {
			Expect(k8sClient.Create(context.TODO(), monitor)).To(Succeed())

			cmKey := types.NamespacedName{
				Name:      builder.ResourceName("test-db") + "-config",
				Namespace: ns.Name,
			}
			Eventually(func() error {
				return k8sClient.Get(context.TODO(), cmKey, &corev1.ConfigMap{})
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
			Expect(k8sClient.Create(context.TODO(), monitor)).To(Succeed())

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

		It("should set DatabaseReachable condition to True", func() {
			Expect(k8sClient.Create(context.TODO(), monitor)).To(Succeed())

			Eventually(func() bool {
				var m pgpilotv1.PgpilotMonitor
				if err := k8sClient.Get(context.TODO(), types.NamespacedName{Name: "test-db", Namespace: ns.Name}, &m); err != nil {
					return false
				}
				for _, c := range m.Status.Conditions {
					if c.Type == "DatabaseReachable" && c.Status == metav1.ConditionTrue {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())
		})

		It("should set finalizer on the monitor", func() {
			Expect(k8sClient.Create(context.TODO(), monitor)).To(Succeed())

			Eventually(func() bool {
				var m pgpilotv1.PgpilotMonitor
				if err := k8sClient.Get(context.TODO(), types.NamespacedName{Name: "test-db", Namespace: ns.Name}, &m); err != nil {
					return false
				}
				for _, f := range m.Finalizers {
					if f == finalizerName {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())
		})

		It("should set OwnerReference on child resources", func() {
			Expect(k8sClient.Create(context.TODO(), monitor)).To(Succeed())

			cmKey := types.NamespacedName{
				Name:      builder.ResourceName("test-db") + "-config",
				Namespace: ns.Name,
			}
			Eventually(func() bool {
				var cm corev1.ConfigMap
				if err := k8sClient.Get(context.TODO(), cmKey, &cm); err != nil {
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
			Expect(k8sClient.Create(context.TODO(), monitor)).To(Succeed())

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

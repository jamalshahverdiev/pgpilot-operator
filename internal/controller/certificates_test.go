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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/url"
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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
	"github.com/jamalshahverdiev/pgpilot-operator/internal/builder"
)

// testCertificate returns a self-signed certificate and its key as PEM.
func testCertificate(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	certPEM, keyPEM, err := newCertificate()
	if err != nil {
		t.Fatal(err)
	}
	return certPEM, keyPEM
}

func newCertificate() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "unit test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), nil
}

func secretOf(name string, data map[string][]byte) *corev1.Secret {
	secret := &corev1.Secret{Data: data}
	secret.Name, secret.Namespace = name, "team"
	return secret
}

// tlsUnitMonitor is the unit monitor with every certificate the API knows.
func tlsUnitMonitor() *pgpilotv1.PgpilotMonitor {
	monitor := unitMonitor()
	monitor.Spec.Database.SSLMode = pgpilotv1.SSLModeVerifyFull
	monitor.Spec.Database.TLS = &pgpilotv1.DatabaseTLS{
		CASecretRef:         &pgpilotv1.CASecretRef{Name: "pg-ca"},
		ClientCertSecretRef: &pgpilotv1.ClientCertSecretRef{Name: "pg-client"},
	}
	monitor.Spec.Sinks.GRPC = &pgpilotv1.GRPCSink{
		Enabled: true, Endpoint: "receiver:9000", TLS: &pgpilotv1.GRPCTLS{CASecretRef: "receiver-ca"},
	}
	return monitor
}

// certificateSecrets returns the three Secrets tlsUnitMonitor names, in
// order: pg-ca, pg-client, receiver-ca.
func certificateSecrets(t *testing.T) []client.Object {
	t.Helper()
	cert, key := testCertificate(t)
	return []client.Object{
		secretOf("pg-ca", map[string][]byte{"ca.crt": cert}),
		secretOf("pg-client", map[string][]byte{"tls.crt": cert, "tls.key": key}),
		secretOf("receiver-ca", map[string][]byte{"ca.crt": cert}),
	}
}

func credentialsVersionOf(h *reconcileHarness) string {
	h.t.Helper()
	return h.storedDeployment().Spec.Template.Annotations[builder.AnnotationCredentialsVersion]
}

// A pod that names a Secret or a key that is not there does not start, and
// one that is handed something that is no certificate fails where only its
// log says so. Both are for the monitor to say, before a pod is made.
func TestReconcile_CertificatesThatCannotBeUsed(t *testing.T) {
	cert, key := testCertificate(t)
	otherCert, _ := testCertificate(t)
	replace := func(objs []client.Object, with *corev1.Secret) []client.Object {
		out := make([]client.Object, 0, len(objs))
		for _, obj := range objs {
			if obj.GetName() != with.Name {
				out = append(out, obj)
			}
		}
		if with.Data != nil {
			out = append(out, with)
		}
		return out
	}

	cases := map[string]struct {
		secret      *corev1.Secret // replaces the one of its name; without data: removes it
		wantReason  string
		wantMessage string
	}{
		"the database's authority is missing": {
			secretOf("pg-ca", nil), "SecretNotFound",
			`Secret "pg-ca", named by spec.database.tls.caSecretRef, was not found in namespace "team"`,
		},
		"the client certificate is missing": {
			secretOf("pg-client", nil), "SecretNotFound",
			`Secret "pg-client", named by spec.database.tls.clientCertSecretRef, was not found`,
		},
		"the receiver's authority is missing": {
			secretOf("receiver-ca", nil), "SecretNotFound",
			`Secret "receiver-ca", named by spec.sinks.grpc.tls.caSecretRef, was not found`,
		},
		"the authority is under another key": {
			secretOf("pg-ca", map[string][]byte{"root.pem": cert}), "SecretKeyMissing",
			`Secret "pg-ca", named by spec.database.tls.caSecretRef, has no value under key "ca.crt"`,
		},
		"the key of the client certificate is missing": {
			secretOf("pg-client", map[string][]byte{"tls.crt": cert}), "SecretKeyMissing",
			`Secret "pg-client", named by spec.database.tls.clientCertSecretRef, has no value under key "tls.key"`,
		},
		"an empty value": {
			secretOf("receiver-ca", map[string][]byte{"ca.crt": {}}), "SecretKeyMissing",
			`has no value under key "ca.crt"`,
		},
		"an authority that is no certificate": {
			secretOf("pg-ca", map[string][]byte{"ca.crt": []byte("LS0tLS1CRUdJTi... encoded twice")}), "InvalidCertificate",
			`Secret "pg-ca", named by spec.database.tls.caSecretRef, cannot be used: key "ca.crt" holds no PEM certificate`,
		},
		"a key where the authority should be": {
			secretOf("receiver-ca", map[string][]byte{"ca.crt": key}), "InvalidCertificate",
			`key "ca.crt" holds no PEM certificate`,
		},
		"a certificate and a key that do not belong together": {
			secretOf("pg-client", map[string][]byte{"tls.crt": otherCert, "tls.key": key}), "InvalidCertificate",
			`Secret "pg-client", named by spec.database.tls.clientCertSecretRef, cannot be used: keys "tls.crt" and ` +
				`"tls.key" do not hold a PEM certificate and, as the first PEM block, its private key: ` +
				`the key is not the one of the certificate`,
		},
		"certificate and key the wrong way round": {
			secretOf("pg-client", map[string][]byte{"tls.crt": key, "tls.key": cert}), "InvalidCertificate",
			`Secret "pg-client", named by spec.database.tls.clientCertSecretRef, cannot be used: key "tls.crt" ` +
				`holds no PEM certificate`,
		},
		"a key that is no PEM": {
			secretOf("pg-client", map[string][]byte{"tls.crt": cert, "tls.key": []byte("0\x82\x04\xbd DER")}), "InvalidCertificate",
			`key "tls.key" holds no PEM block`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			objs := replace(certificateSecrets(t), tc.secret)
			if tc.secret.Name == "pg-client" && tc.secret.Data != nil && tc.wantReason == "InvalidCertificate" {
				// The pair of this test, not of certificateSecrets.
				objs = replace(objs, tc.secret)
			}
			h := newReconcileHarness(t, tlsUnitMonitor(), append(objs, unitSecret())...)
			result, err := h.reconcile()
			// Nothing watches Secrets: the operator looks again.
			if err != nil || result.RequeueAfter != h.r.credentialsRetry() {
				t.Errorf("got %+v, %v", result, err)
			}
			if len(h.applied) != 0 {
				t.Errorf("applied %v for a pod that could not start, or could not work", h.applied)
			}
			for _, condition := range []string{"Ready", "CredentialsAvailable"} {
				h.expectCondition(condition, metav1.ConditionFalse, tc.wantReason, tc.wantMessage)
			}
			// Whatever is wrong with a Secret, nothing of what it holds is
			// repeated.
			events := drain(h.recorder)
			if len(events) != 1 {
				t.Errorf("events %q, want the one that says what is wrong", events)
			}
			for _, e := range events {
				if strings.Contains(e, "BEGIN") || strings.Contains(e, "LS0tLS1") || strings.Contains(e, "tls:") {
					t.Errorf("the event repeats what the Secret holds: %s", e)
				}
			}

			// The operator looks again every half minute for as long as
			// it takes. Nothing has changed, so nothing is written and
			// nothing is said: the condition keeps the time at which the
			// trouble began.
			since := meta.FindStatusCondition(h.monitor().Status.Conditions, "CredentialsAvailable").LastTransitionTime
			writes := h.statusWrites
			time.Sleep(1100 * time.Millisecond) // the times of a condition are whole seconds
			if _, err := h.reconcile(); err != nil {
				t.Fatal(err)
			}
			if again := drain(h.recorder); len(again) != 0 || h.statusWrites != writes {
				t.Errorf("reported again: events %q, %d status writes", again, h.statusWrites-writes)
			}
			now := meta.FindStatusCondition(h.monitor().Status.Conditions, "CredentialsAvailable").LastTransitionTime
			if !now.Equal(&since) {
				t.Errorf("CredentialsAvailable went from %s to %s although nothing changed", since, now)
			}
		})
	}
}

// With inline credentials the same holds: the Secret generated for them is
// written, and found unchanged, at every look, and the condition must not go
// to True for it and back for the certificate.
func TestReconcile_AMissingCertificateOfAnInlineMonitorIsReportedOnce(t *testing.T) {
	monitor := inlineUnitMonitor("s3cret")
	monitor.Spec.Database.TLS = &pgpilotv1.DatabaseTLS{CASecretRef: &pgpilotv1.CASecretRef{Name: "pg-ca"}}
	h := newReconcileHarness(t, monitor)
	if result, err := h.reconcile(); err != nil || result.RequeueAfter != h.r.credentialsRetry() {
		t.Fatalf("got %+v, %v", result, err)
	}
	h.expectCondition("CredentialsAvailable", metav1.ConditionFalse, "SecretNotFound", `Secret "pg-ca"`)
	if got := strings.Join(h.applied, ","); got != "Secret" {
		t.Errorf("applied %q, want the generated Secret alone", got)
	}
	drain(h.recorder)
	since := meta.FindStatusCondition(h.monitor().Status.Conditions, "CredentialsAvailable").LastTransitionTime
	writes := h.statusWrites
	time.Sleep(1100 * time.Millisecond) // the times of a condition are whole seconds
	for range 2 {
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
	}
	if again := drain(h.recorder); len(again) != 0 || h.statusWrites != writes {
		t.Errorf("reported again: events %q, %d status writes", again, h.statusWrites-writes)
	}
	now := meta.FindStatusCondition(h.monitor().Status.Conditions, "CredentialsAvailable").LastTransitionTime
	if !now.Equal(&since) {
		t.Errorf("CredentialsAvailable went from %s to %s although nothing changed", since, now)
	}
}

// What is asked of the API server and cannot be had: a name that no Secret
// can have, and a Secret that cannot be read.
func TestReconcile_CertificateSecretsThatCannotBeAskedFor(t *testing.T) {
	// The field of the gRPC sink takes anything. A certificate pasted into
	// it is refused as a name, and not repeated: a message of that size is
	// one the API server would not store.
	t.Run("a name that is no name at all", func(t *testing.T) {
		monitor := tlsUnitMonitor()
		monitor.Spec.Sinks.GRPC.TLS.CASecretRef = strings.Repeat("MIIBszCCAVmgAwIBAgIU", 2000)
		h := newReconcileHarness(t, monitor, append(certificateSecrets(t), unitSecret())...)
		if result, err := h.reconcile(); err != nil || result.RequeueAfter != 0 {
			t.Errorf("got %+v, %v", result, err)
		}
		h.expectCondition("Ready", metav1.ConditionFalse, "SecretNotFound",
			`Secret "`+strings.Repeat("MIIBszCCAVmgAwIBAgIU", 4)[:63]+`... (40000 characters)", named by spec.sinks.grpc.tls.caSecretRef,`)
		for _, condition := range h.monitor().Status.Conditions {
			if len(condition.Message) > 1024 {
				t.Errorf("%s has a message of %d characters", condition.Type, len(condition.Message))
			}
		}
		for _, e := range drain(h.recorder) {
			if len(e) > 1024 {
				t.Errorf("an event of %d characters", len(e))
			}
		}
	})

	// The name of the gRPC sink's authority has no rule in the API: it was
	// stored before it meant anything. One that could never be a Secret's is
	// a mistake in the monitor, not a Secret the operator fails to read.
	for name, value := range map[string]string{
		"another namespace": "monitoring/receiver-ca", "dots": "..", "an escape": "a%2Fb",
		"upper case": "Receiver-CA", "too long": strings.Repeat("a", 254),
	} {
		t.Run("a name that is no Secret's: "+name, func(t *testing.T) {
			monitor := tlsUnitMonitor()
			monitor.Spec.Sinks.GRPC.TLS.CASecretRef = value
			h := newReconcileHarness(t, monitor, append(certificateSecrets(t), unitSecret())...)
			secretReads := 0
			h.getError = func(obj client.Object) error {
				if _, isSecret := obj.(*corev1.Secret); isSecret {
					secretReads++
				}
				return nil
			}
			result, err := h.reconcile()
			// Looking again does not change the monitor's spec.
			if err != nil || result.RequeueAfter != 0 {
				t.Errorf("got %+v, %v", result, err)
			}
			// db-creds, pg-ca, pg-client, and no question that the client
			// would refuse to ask.
			if secretReads != 3 || len(h.applied) != 0 {
				t.Errorf("%d Secrets read, applied %v", secretReads, h.applied)
			}
			for _, condition := range []string{"Ready", "CredentialsAvailable"} {
				h.expectCondition(condition, metav1.ConditionFalse, "SecretNotFound",
					"named by spec.sinks.grpc.tls.caSecretRef, cannot exist: that is not the name of a Secret (")
			}
		})
	}

	t.Run("a Secret that cannot be read is the operator's failure", func(t *testing.T) {
		h := newReconcileHarness(t, tlsUnitMonitor(), append(certificateSecrets(t), unitSecret())...)
		boom := apierrors.NewServiceUnavailable("etcd leader changed")
		// The hook does not see the name that is asked for. The Secrets are
		// read in a fixed order: db-creds, pg-ca, pg-client.
		secretReads := 0
		h.getError = func(obj client.Object) error {
			if _, isSecret := obj.(*corev1.Secret); isSecret {
				if secretReads++; secretReads == 3 {
					return boom
				}
			}
			return nil
		}
		result, err := h.reconcile()
		if !errors.Is(err, boom) || result.RequeueAfter != 0 {
			t.Errorf("got %+v, %v", result, err)
		}
		h.expectCondition("Ready", metav1.ConditionFalse, "SecretReadFailed",
			`Secret "pg-client", named by spec.database.tls.clientCertSecretRef, could not be read`)
	})
}

func TestReconcile_CertificatesAreMountedAndFollowed(t *testing.T) {
	t.Run("the pod mounts them, and the monitor's credentials condition stays what it was", func(t *testing.T) {
		h := newReconcileHarness(t, tlsUnitMonitor(), append(certificateSecrets(t), unitSecret())...)
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		h.expectCondition("CredentialsAvailable", metav1.ConditionTrue, "CredentialsFound",
			`Secret "db-creds" provides keys "username" and "password"; certificates are mounted from `+
				`"pg-ca", "pg-client", "receiver-ca"`)
		mounted := map[string]bool{}
		for _, volume := range h.storedDeployment().Spec.Template.Spec.Volumes {
			if volume.Secret != nil {
				mounted[volume.Secret.SecretName] = true
			}
		}
		if len(mounted) != 3 || !mounted["pg-ca"] || !mounted["pg-client"] || !mounted["receiver-ca"] {
			t.Errorf("mounted Secrets %v", mounted)
		}
	})

	// pgwatch reads the files when it starts. A renewed certificate takes a
	// new pod, and the token on the pod template is what brings one.
	t.Run("a Secret that changes replaces the pod, each of them", func(t *testing.T) {
		h := newReconcileHarness(t, tlsUnitMonitor(), append(certificateSecrets(t), unitSecret())...)
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		version := credentialsVersionOf(h)
		if version == "" {
			t.Fatal("no credentials version on the pod template")
		}
		for _, name := range []string{"db-creds", "pg-ca", "pg-client", "receiver-ca"} {
			secret, err := h.storedSecret(name)
			if err != nil {
				t.Fatal(err)
			}
			secret.Labels = map[string]string{"renewed": name}
			if err := h.client.Update(context.Background(), secret); err != nil {
				t.Fatal(err)
			}
			if _, err := h.reconcile(); err != nil {
				t.Fatal(err)
			}
			if got := credentialsVersionOf(h); got == version {
				t.Errorf("Secret %s changed and the pod template did not", name)
			} else {
				version = got
			}
		}
		// Nothing changed: nothing to replace.
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		if got := credentialsVersionOf(h); got != version {
			t.Errorf("the credentials version moved from %q to %q although no Secret changed", version, got)
		}
	})

	// Nothing tells the operator about a changed Secret. A monitor with
	// inline credentials is not looked at again on its own; one that mounts
	// certificates has to be.
	t.Run("a monitor with inline credentials is looked at again for its certificates", func(t *testing.T) {
		cert, _ := testCertificate(t)
		monitor := inlineUnitMonitor("s3cret")
		monitor.Spec.Database.TLS = &pgpilotv1.DatabaseTLS{CASecretRef: &pgpilotv1.CASecretRef{Name: "pg-ca"}}
		h := newReconcileHarness(t, monitor, secretOf("pg-ca", map[string][]byte{"ca.crt": cert}), runningPod("pgpilot-db-abc"))
		h.deploymentStatus = rolledOut
		result, err := h.reconcile()
		if err != nil || result.RequeueAfter <= 0 {
			t.Errorf("got %+v, %v; want another look", result, err)
		}
		h.expectCondition("Ready", metav1.ConditionTrue, "PodRunning", "")
	})

	// A monitor without certificates gets the token it always had: a new
	// one would replace every collector at the upgrade for nothing.
	t.Run("without certificates the token is that of the credentials Secret alone", func(t *testing.T) {
		h := newReconcileHarness(t, unitMonitor(), unitSecret())
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		secret, err := h.storedSecret("db-creds")
		if err != nil {
			t.Fatal(err)
		}
		if got, want := credentialsVersionOf(h), credentialsVersion(secret); got != want {
			t.Errorf("credentials version %q, want that of the credentials Secret, %q", got, want)
		}
	})

	// The condition is set once, when everything has been found: what it
	// said about a certificate is gone with the trouble.
	t.Run("a certificate that is put right", func(t *testing.T) {
		cert, key := testCertificate(t)
		monitor := unitMonitor()
		monitor.Spec.Database.TLS = &pgpilotv1.DatabaseTLS{CASecretRef: &pgpilotv1.CASecretRef{Name: "pg-ca"}}
		h := newReconcileHarness(t, monitor, unitSecret(), secretOf("pg-ca", map[string][]byte{"ca.crt": key}))
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		h.expectCondition("CredentialsAvailable", metav1.ConditionFalse, "InvalidCertificate", "")
		authority, err := h.storedSecret("pg-ca")
		if err != nil {
			t.Fatal(err)
		}
		authority.Data = map[string][]byte{"ca.crt": cert}
		if err := h.client.Update(context.Background(), authority); err != nil {
			t.Fatal(err)
		}
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		h.expectCondition("CredentialsAvailable", metav1.ConditionTrue, "CredentialsFound",
			`; certificates are mounted from "pg-ca"`)
	})

	// One Secret for the authority and the client certificate is named once.
	t.Run("one Secret for all of it", func(t *testing.T) {
		cert, key := testCertificate(t)
		monitor := inlineUnitMonitor("s3cret")
		monitor.Spec.Database.TLS = &pgpilotv1.DatabaseTLS{
			CASecretRef:         &pgpilotv1.CASecretRef{Name: "pg-tls"},
			ClientCertSecretRef: &pgpilotv1.ClientCertSecretRef{Name: "pg-tls"},
		}
		h := newReconcileHarness(t, monitor,
			secretOf("pg-tls", map[string][]byte{"ca.crt": cert, "tls.crt": cert, "tls.key": key}))
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		h.expectCondition("CredentialsAvailable", metav1.ConditionTrue, "InlineCredentials",
			`the collector reads them from Secret "pgpilot-db-credentials"; certificates are mounted from "pg-tls"`)
		if got := h.monitor().Status.Conditions; strings.Count(meta.FindStatusCondition(got, "CredentialsAvailable").Message, "pg-tls") != 1 {
			t.Errorf("conditions %+v", got)
		}
	})

	t.Run("a key of the monitor's choosing", func(t *testing.T) {
		cert, _ := testCertificate(t)
		monitor := unitMonitor()
		monitor.Spec.Database.TLS = &pgpilotv1.DatabaseTLS{CASecretRef: &pgpilotv1.CASecretRef{Name: "pg-ca", Key: "root.pem"}}
		h := newReconcileHarness(t, monitor, unitSecret(), secretOf("pg-ca", map[string][]byte{"root.pem": cert}))
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(h.applied, ","); got != "ConfigMap,Deployment,Service" {
			t.Errorf("applied %q", got)
		}
	})
}

var _ = Describe("The collector's certificates", func() {
	const monitorName = "tls-db"
	var (
		ns         string
		monitorKey types.NamespacedName
		deployKey  types.NamespacedName
		cert, key  []byte
	)

	BeforeEach(func() {
		ns = testNamespace("test-tls-")
		monitorKey = types.NamespacedName{Name: monitorName, Namespace: ns}
		deployKey = types.NamespacedName{Name: "pgpilot-" + monitorName, Namespace: ns}
		var err error
		cert, key, err = newCertificate()
		Expect(err).NotTo(HaveOccurred())
	})

	secret := func(name string, data map[string][]byte) *corev1.Secret {
		s := &corev1.Secret{Data: data}
		s.Name, s.Namespace = name, ns
		return s
	}
	// newMonitor is a monitor that checks its server against an authority.
	newMonitor := func() *pgpilotv1.PgpilotMonitor {
		m := &pgpilotv1.PgpilotMonitor{}
		m.Name, m.Namespace = monitorName, ns
		m.Spec.Database = pgpilotv1.DatabaseRef{
			Host: "pg.example.com", Database: "appdb", SSLMode: pgpilotv1.SSLModeVerifyFull,
			CredentialsSecret: &pgpilotv1.CredentialsSecretRef{Name: "pg-creds"},
			TLS:               &pgpilotv1.DatabaseTLS{CASecretRef: &pgpilotv1.CASecretRef{Name: "pg-ca"}},
		}
		m.Spec.Metrics = pgpilotv1.MetricsSpec{Preset: "basic"}
		m.Spec.Sinks = pgpilotv1.SinksSpec{Prometheus: &pgpilotv1.PrometheusSink{Enabled: true}}
		return m
	}
	expectUnavailable := func(reason, messagePart string) {
		GinkgoHelper()
		Eventually(func(g Gomega) {
			var m pgpilotv1.PgpilotMonitor
			g.Expect(k8sClient.Get(context.TODO(), monitorKey, &m)).To(Succeed())
			for _, conditionType := range []string{conditionCredentials, "Ready"} {
				c := meta.FindStatusCondition(m.Status.Conditions, conditionType)
				g.Expect(c).NotTo(BeNil(), conditionType)
				g.Expect(c.Status).To(Equal(metav1.ConditionFalse), conditionType)
				g.Expect(c.Reason).To(Equal(reason), conditionType)
				g.Expect(c.Message).To(ContainSubstring(messagePart), conditionType)
			}
		}, timeout, interval).Should(Succeed())
	}
	deployment := func(g Gomega) *appsv1.Deployment {
		var dep appsv1.Deployment
		g.Expect(k8sClient.Get(context.TODO(), deployKey, &dep)).To(Succeed())
		return &dep
	}
	version := func(g Gomega) string {
		return deployment(g).Spec.Template.Annotations[builder.AnnotationCredentialsVersion]
	}
	// becomeReady reports the collector pod as running, which nothing in
	// this API server does: only a ready monitor is on the periodic look at
	// its Secrets.
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
			g.Expect(m.Status.Ready).To(BeTrue())
		}, timeout, interval).Should(Succeed())
	}
	withinRechecks := 4 * testCredentialsRecheck

	// Nothing watches Secrets: one that is created, or put right, after the
	// monitor is found by looking again.
	It("waits for a certificate that is missing or no certificate, and starts once it is there", func() {
		createForSpec(secret("pg-creds", map[string][]byte{"username": []byte("pgwatch"), "password": []byte("s3cret")}))
		createForSpec(newMonitor())
		expectUnavailable("SecretNotFound", `Secret "pg-ca", named by spec.database.tls.caSecretRef, was not found`)
		var dep appsv1.Deployment
		Expect(apierrors.IsNotFound(k8sClient.Get(context.TODO(), deployKey, &dep))).To(BeTrue(),
			"a pod that mounts a Secret that is not there does not start")

		// The certificate, encoded once too often: what `kubectl create
		// secret --from-literal` of base64 text leaves.
		authority := secret("pg-ca", map[string][]byte{"ca.crt": []byte("LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0t")})
		createForSpec(authority)
		expectUnavailable("InvalidCertificate", `key "ca.crt" holds no PEM certificate`)
		Expect(apierrors.IsNotFound(k8sClient.Get(context.TODO(), deployKey, &dep))).To(BeTrue())

		authority.Data = map[string][]byte{"ca.crt": cert}
		Expect(k8sClient.Update(context.TODO(), authority)).To(Succeed())
		Eventually(func(g Gomega) {
			pod := deployment(g).Spec.Template.Spec
			g.Expect(pod.Volumes).To(HaveLen(3))
			g.Expect(pod.Volumes[2].Secret).NotTo(BeNil())
			g.Expect(pod.Volumes[2].Secret.SecretName).To(Equal("pg-ca"))
			g.Expect(pod.Containers[0].VolumeMounts[2].ReadOnly).To(BeTrue())

			cm, err := mountedConfigMap(k8sClient, monitorKey)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cm.Data[builder.SourcesKey]).To(ContainSubstring(
				"sslrootcert=" + url.QueryEscape(pod.Containers[0].VolumeMounts[2].MountPath+"/ca.crt")))
			g.Expect(cm.Data[builder.SourcesKey]).To(ContainSubstring("sslmode=verify-full"))
		}, timeout, interval).Should(Succeed())
		Eventually(func(g Gomega) {
			var m pgpilotv1.PgpilotMonitor
			g.Expect(k8sClient.Get(context.TODO(), monitorKey, &m)).To(Succeed())
			c := meta.FindStatusCondition(m.Status.Conditions, conditionCredentials)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Status).To(Equal(metav1.ConditionTrue))
		}, timeout, interval).Should(Succeed())
	})

	// pgwatch reads the files when it starts. cert-manager renews a
	// certificate by writing its Secret, and nothing else happens: the
	// operator has to see it and replace the pod. A monitor with inline
	// credentials is otherwise not looked at again at all.
	DescribeTable("replaces the pod of a healthy monitor when a certificate is renewed",
		func(inline bool) {
			monitor := newMonitor()
			monitor.Spec.Database.TLS.ClientCertSecretRef = &pgpilotv1.ClientCertSecretRef{Name: "pg-client"}
			if inline {
				monitor.Spec.Database.CredentialsSecret = nil
				monitor.Spec.Database.Username, monitor.Spec.Database.Password = "pgwatch", "s3cret"
			} else {
				createForSpec(secret("pg-creds", map[string][]byte{"username": []byte("pgwatch"), "password": []byte("s3cret")}))
			}
			createForSpec(secret("pg-ca", map[string][]byte{"ca.crt": cert}))
			pair := secret("pg-client", map[string][]byte{"tls.crt": cert, "tls.key": key})
			pair.Type = corev1.SecretTypeTLS
			createForSpec(pair)
			createForSpec(monitor)
			becomeReady()

			var before string
			var generation int64
			Eventually(func(g Gomega) {
				before = version(g)
				g.Expect(before).NotTo(BeEmpty())
				generation = deployment(g).Generation
			}, timeout, interval).Should(Succeed())
			// Looked at again and again, an unchanged Secret changes nothing.
			Consistently(func(g Gomega) {
				g.Expect(version(g)).To(Equal(before))
				g.Expect(deployment(g).Generation).To(Equal(generation))
			}, 2*testCredentialsRecheck, interval).Should(Succeed())

			renewedCert, renewedKey, err := newCertificate()
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(context.TODO(), client.ObjectKeyFromObject(pair), pair)).To(Succeed())
			pair.Data = map[string][]byte{"tls.crt": renewedCert, "tls.key": renewedKey}
			Expect(k8sClient.Update(context.TODO(), pair)).To(Succeed())

			Eventually(func(g Gomega) {
				g.Expect(version(g)).NotTo(Equal(before))
				g.Expect(deployment(g).Generation).To(BeNumerically(">", generation),
					"the pod template must have changed for the pod to be replaced")
			}, withinRechecks, interval).Should(Succeed())
		},
		Entry("with a credentials Secret", false),
		Entry("with inline credentials", true),
	)

	// The password of a monitor with inline credentials is not in its pod
	// template. Its generated Secret is one of the things the token of a
	// monitor with certificates is made of, and the only one that replaces
	// the pod when the password changes.
	It("replaces the pod when an inline password changes under certificates", func() {
		monitor := newMonitor()
		monitor.Spec.Database.CredentialsSecret = nil
		monitor.Spec.Database.Username, monitor.Spec.Database.Password = "pgwatch", "old"
		createForSpec(secret("pg-ca", map[string][]byte{"ca.crt": cert}))
		createForSpec(monitor)
		becomeReady()
		var before string
		Eventually(func(g Gomega) { before = version(g); g.Expect(before).NotTo(BeEmpty()) }, timeout, interval).Should(Succeed())
		Consistently(func(g Gomega) { g.Expect(version(g)).To(Equal(before)) }, testCredentialsRecheck, interval).Should(Succeed())

		Eventually(func(g Gomega) {
			var m pgpilotv1.PgpilotMonitor
			g.Expect(k8sClient.Get(context.TODO(), monitorKey, &m)).To(Succeed())
			m.Spec.Database.Password = "rotated"
			g.Expect(k8sClient.Update(context.TODO(), &m)).To(Succeed())
		}, timeout, interval).Should(Succeed())
		Eventually(func(g Gomega) { g.Expect(version(g)).NotTo(Equal(before)) }, timeout, interval).Should(Succeed())
	})

	// A certificate and its key in one value of the Secret: the one key is
	// mounted under two names, which the API server has to take.
	It("mounts one value as the certificate and as its key", func() {
		createForSpec(secret("pg-creds", map[string][]byte{"username": []byte("pgwatch"), "password": []byte("s3cret")}))
		createForSpec(secret("pg-ca", map[string][]byte{"ca.crt": cert}))
		createForSpec(secret("pg-client", map[string][]byte{"client.pem": append(append([]byte{}, key...), cert...)}))
		monitor := newMonitor()
		monitor.Spec.Database.TLS.ClientCertSecretRef = &pgpilotv1.ClientCertSecretRef{
			Name: "pg-client", CertKey: "client.pem", KeyKey: "client.pem",
		}
		createForSpec(monitor)
		Eventually(func(g Gomega) {
			volumes := deployment(g).Spec.Template.Spec.Volumes
			g.Expect(volumes).To(HaveLen(4))
			g.Expect(volumes[3].Secret).NotTo(BeNil())
			g.Expect(volumes[3].Secret.Items).To(Equal([]corev1.KeyToPath{
				{Key: "client.pem", Path: "tls.crt"}, {Key: "client.pem", Path: "tls.key"},
			}))
		}, timeout, interval).Should(Succeed())
	})

	// A value stored in the field of the gRPC sink before it meant anything
	// is not refused by the API server, and is the monitor's to put right.
	It("says of a name that is no Secret's that there is no such Secret", func() {
		createForSpec(secret("pg-creds", map[string][]byte{"username": []byte("pgwatch"), "password": []byte("s3cret")}))
		createForSpec(secret("pg-ca", map[string][]byte{"ca.crt": cert}))
		monitor := newMonitor()
		monitor.Spec.Sinks.GRPC = &pgpilotv1.GRPCSink{
			Enabled: true, Endpoint: "receiver:9000", TLS: &pgpilotv1.GRPCTLS{CASecretRef: "monitoring/receiver-ca"},
		}
		createForSpec(monitor)
		expectUnavailable("SecretNotFound",
			`Secret "monitoring/receiver-ca", named by spec.sinks.grpc.tls.caSecretRef, cannot exist`)
	})

	// A renewal that goes wrong must not take the running collector down:
	// the pod that has the old certificate keeps it.
	It("leaves the running collector alone when a renewed Secret cannot be used", func() {
		createForSpec(secret("pg-creds", map[string][]byte{"username": []byte("pgwatch"), "password": []byte("s3cret")}))
		authority := secret("pg-ca", map[string][]byte{"ca.crt": cert})
		createForSpec(authority)
		createForSpec(newMonitor())
		becomeReady()
		var before string
		Eventually(func(g Gomega) { before = version(g); g.Expect(before).NotTo(BeEmpty()) }, timeout, interval).Should(Succeed())

		Expect(k8sClient.Get(context.TODO(), client.ObjectKeyFromObject(authority), authority)).To(Succeed())
		authority.Data = map[string][]byte{"ca.crt": key}
		Expect(k8sClient.Update(context.TODO(), authority)).To(Succeed())
		Eventually(func(g Gomega) {
			var m pgpilotv1.PgpilotMonitor
			g.Expect(k8sClient.Get(context.TODO(), monitorKey, &m)).To(Succeed())
			c := meta.FindStatusCondition(m.Status.Conditions, "Ready")
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Reason).To(Equal("InvalidCertificate"))
		}, withinRechecks, interval).Should(Succeed())
		Consistently(func(g Gomega) { g.Expect(version(g)).To(Equal(before)) }, time.Second, interval).Should(Succeed())
	})
})

// The Secret the operator generates for inline credentials is removed when
// the monitor has none. A pod that mounted a certificate from it would lose
// it then, so it cannot be named for one; a Secret of that name that is the
// user's own is as good as any other.
func TestReconcile_TheGeneratedSecretHoldsNoCertificate(t *testing.T) {
	cert, _ := testCertificate(t)
	for field, tlsOf := range map[string]func(*pgpilotv1.PgpilotMonitor){
		"spec.database.tls.caSecretRef": func(m *pgpilotv1.PgpilotMonitor) {
			m.Spec.Database.TLS = &pgpilotv1.DatabaseTLS{
				CASecretRef: &pgpilotv1.CASecretRef{Name: "pgpilot-db-credentials", Key: "password"},
			}
		},
		"spec.database.tls.clientCertSecretRef": func(m *pgpilotv1.PgpilotMonitor) {
			m.Spec.Database.TLS = &pgpilotv1.DatabaseTLS{ClientCertSecretRef: &pgpilotv1.ClientCertSecretRef{
				Name: "pgpilot-db-credentials", CertKey: "username", KeyKey: "password",
			}}
		},
		"spec.sinks.grpc.tls.caSecretRef": func(m *pgpilotv1.PgpilotMonitor) {
			m.Spec.Sinks.GRPC = &pgpilotv1.GRPCSink{
				Enabled: true, Endpoint: "receiver:9000", TLS: &pgpilotv1.GRPCTLS{CASecretRef: "pgpilot-db-credentials"},
			}
		},
	} {
		t.Run(field, func(t *testing.T) {
			monitor := inlineUnitMonitor("s3cret")
			tlsOf(monitor)
			h := newReconcileHarness(t, monitor)
			result, err := h.reconcile()
			// A mistake in the spec: looking again does not change it.
			if err != nil || result.RequeueAfter != 0 {
				t.Errorf("got %+v, %v", result, err)
			}
			if got := strings.Join(h.applied, ","); got != "Secret" {
				t.Errorf("applied %q, want the generated Secret and no pod that mounts it", got)
			}
			for _, condition := range []string{"Ready", "CredentialsAvailable"} {
				h.expectCondition(condition, metav1.ConditionFalse, "GeneratedSecretReferenced",
					`Secret "pgpilot-db-credentials", named by `+field+`, is the one the operator generates from inline `+
						`credentials, and removes once a monitor has none. It cannot hold a certificate of the monitor`)
			}
		})
	}

	// The case that loses a pod its certificate: the monitor has turned to a
	// Secret of its own, the one generated for it is still there and about
	// to go, and a certificate is looked for in it.
	t.Run("after the monitor has turned to a Secret of its own", func(t *testing.T) {
		h := switched(t, unitSecret())
		stored := h.monitor()
		stored.Spec.Database.TLS = &pgpilotv1.DatabaseTLS{
			CASecretRef: &pgpilotv1.CASecretRef{Name: "pgpilot-db-credentials", Key: "password"},
		}
		stored.Generation++
		if err := h.client.Update(context.Background(), stored); err != nil {
			t.Fatal(err)
		}
		result, err := h.reconcile()
		if err != nil || result.RequeueAfter != 0 {
			t.Errorf("got %+v, %v", result, err)
		}
		if len(h.applied) != 0 {
			t.Errorf("applied %v", h.applied)
		}
		h.expectCondition("Ready", metav1.ConditionFalse, "GeneratedSecretReferenced",
			`named by spec.database.tls.caSecretRef, is the one the operator generates`)
		stillThere(t, h)
	})

	t.Run("a Secret of that name that is the user's own", func(t *testing.T) {
		monitor := unitMonitor()
		monitor.Spec.Database.TLS = &pgpilotv1.DatabaseTLS{CASecretRef: &pgpilotv1.CASecretRef{Name: "pgpilot-db-credentials"}}
		h := newReconcileHarness(t, monitor, unitSecret(),
			secretOf("pgpilot-db-credentials", map[string][]byte{"ca.crt": cert}))
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		h.expectCondition("CredentialsAvailable", metav1.ConditionTrue, "CredentialsFound", "")
		if got := strings.Join(h.applied, ","); got != "ConfigMap,Deployment,Service" {
			t.Errorf("applied %q", got)
		}
	})
}

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

package builder

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
)

// tlsMonitor is the test monitor with every certificate the API knows.
func tlsMonitor() *pgpilotv1.PgpilotMonitor {
	monitor := newTestMonitor()
	monitor.Spec.Database.SSLMode = pgpilotv1.SSLModeVerifyFull
	monitor.Spec.Database.TLS = &pgpilotv1.DatabaseTLS{
		CASecretRef:         &pgpilotv1.CASecretRef{Name: "pg-ca"},
		ClientCertSecretRef: &pgpilotv1.ClientCertSecretRef{Name: "pg-client"},
	}
	monitor.Spec.Sinks.GRPC = &pgpilotv1.GRPCSink{
		Enabled: true, Endpoint: "receiver.monitoring:9000",
		TLS: &pgpilotv1.GRPCTLS{CASecretRef: "receiver-ca"},
	}
	return monitor
}

// connStrOf returns the connection string of the rendered source, parsed.
func connStrOf(t *testing.T, monitor *pgpilotv1.PgpilotMonitor) *url.URL {
	t.Helper()
	cm, _, err := BuildConfigMap(monitor, MergedMetrics{Preset: "basic"})
	if err != nil {
		t.Fatalf("BuildConfigMap: %v", err)
	}
	var sources []pgwatchSource
	if err := yaml.Unmarshal([]byte(cm.Data[SourcesKey]), &sources); err != nil || len(sources) != 1 {
		t.Fatalf("sources.yaml: %v\n%s", err, cm.Data[SourcesKey])
	}
	parsed, err := url.Parse(sources[0].ConnStr)
	if err != nil {
		t.Fatalf("conn_str %q: %v", sources[0].ConnStr, err)
	}
	return parsed
}

func TestMountedSecrets(t *testing.T) {
	describe := func(monitor *pgpilotv1.PgpilotMonitor) []string {
		secrets := MountedSecrets(monitor)
		out := make([]string, 0, len(secrets))
		for _, s := range secrets {
			out = append(out, s.Field+"="+s.Name+"["+strings.Join(s.Keys, ",")+"]")
		}
		return out
	}

	t.Run("none without a tls block", func(t *testing.T) {
		if got := describe(newTestMonitor()); len(got) != 0 {
			t.Errorf("got %v", got)
		}
	})

	t.Run("all three, in a fixed order, with the default keys", func(t *testing.T) {
		want := []string{
			"spec.database.tls.caSecretRef=pg-ca[ca.crt]",
			"spec.database.tls.clientCertSecretRef=pg-client[tls.crt,tls.key]",
			"spec.sinks.grpc.tls.caSecretRef=receiver-ca[ca.crt]",
		}
		if got := describe(tlsMonitor()); !slices.Equal(got, want) {
			t.Errorf("got  %v\nwant %v", got, want)
		}
	})

	t.Run("keys of the monitor's choosing", func(t *testing.T) {
		monitor := tlsMonitor()
		monitor.Spec.Database.TLS.CASecretRef.Key = "root.pem"
		monitor.Spec.Database.TLS.ClientCertSecretRef.CertKey = "client.pem"
		monitor.Spec.Database.TLS.ClientCertSecretRef.KeyKey = "client.key"
		want := []string{
			"spec.database.tls.caSecretRef=pg-ca[root.pem]",
			"spec.database.tls.clientCertSecretRef=pg-client[client.pem,client.key]",
			"spec.sinks.grpc.tls.caSecretRef=receiver-ca[ca.crt]",
		}
		if got := describe(monitor); !slices.Equal(got, want) {
			t.Errorf("got  %v\nwant %v", got, want)
		}
	})

	// What nothing reads is not mounted: a Secret that is missing would
	// keep a pod from starting for a certificate it has no use for.
	t.Run("the gRPC certificate authority only with a gRPC sink in use", func(t *testing.T) {
		for name, change := range map[string]func(*pgpilotv1.GRPCSink){
			"turned off":       func(g *pgpilotv1.GRPCSink) { g.Enabled = false },
			"no endpoint":      func(g *pgpilotv1.GRPCSink) { g.Endpoint = "" },
			"an empty name":    func(g *pgpilotv1.GRPCSink) { g.TLS.CASecretRef = "" },
			"no tls block":     func(g *pgpilotv1.GRPCSink) { g.TLS = nil },
			"the sink removed": nil,
		} {
			monitor := tlsMonitor()
			if change == nil {
				monitor.Spec.Sinks.GRPC = nil
			} else {
				change(monitor.Spec.Sinks.GRPC)
			}
			if got := describe(monitor); len(got) != 2 || strings.Contains(strings.Join(got, " "), "grpc") {
				t.Errorf("%s: got %v", name, got)
			}
		}
	})

	t.Run("a reference without a name is none", func(t *testing.T) {
		monitor := tlsMonitor()
		monitor.Spec.Database.TLS.CASecretRef = &pgpilotv1.CASecretRef{}
		monitor.Spec.Database.TLS.ClientCertSecretRef = nil
		monitor.Spec.Sinks.GRPC = nil
		if got := describe(monitor); len(got) != 0 {
			t.Errorf("got %v", got)
		}
		// Nor does the connection string name a file that is not mounted.
		monitor.Spec.Database.TLS.ClientCertSecretRef = &pgpilotv1.ClientCertSecretRef{}
		if params := connStrOf(t, monitor).Query(); len(params) != 1 || params.Get("sslmode") == "" {
			t.Errorf("conn_str parameters %v", params)
		}
	})
}

// The pod mounts each Secret read-only under a directory of its own, each
// certificate under a fixed name, and the connection string and the sink
// address name exactly those files.
func TestBuildDeployment_MountsTheCertificates(t *testing.T) {
	monitor := tlsMonitor()
	monitor.Spec.Database.TLS.CASecretRef.Key = "root.pem"
	dep := BuildDeployment(monitor, MergedMetrics{Preset: "basic"}, PodTemplateVersions{Config: "h"})
	pod := dep.Spec.Template.Spec
	container := pod.Containers[0]

	type mounted struct{ secret, key, path string }
	var got []mounted
	for _, volume := range pod.Volumes {
		if volume.Secret == nil {
			continue
		}
		if volume.Secret.DefaultMode == nil || *volume.Secret.DefaultMode != 0o444 {
			t.Errorf("volume %s: mode %v; the collector does not own the files and has to read them", volume.Name, volume.Secret.DefaultMode)
		}
		if volume.Secret.Optional != nil {
			t.Errorf("volume %s is optional: a pod without its certificate would start and fail later", volume.Name)
		}
		var mount *corev1.VolumeMount
		for i := range container.VolumeMounts {
			if container.VolumeMounts[i].Name == volume.Name {
				mount = &container.VolumeMounts[i]
			}
		}
		if mount == nil || !mount.ReadOnly || mount.SubPath != "" {
			t.Fatalf("volume %s: mount %+v", volume.Name, mount)
		}
		for _, item := range volume.Secret.Items {
			got = append(got, mounted{volume.Secret.SecretName, item.Key, mount.MountPath + "/" + item.Path})
		}
	}
	want := []mounted{
		{"pg-ca", "root.pem", "/etc/pgwatch-tls/database-ca/ca.crt"},
		{"pg-client", "tls.crt", "/etc/pgwatch-tls/database-client-cert/tls.crt"},
		{"pg-client", "tls.key", "/etc/pgwatch-tls/database-client-cert/tls.key"},
		{"receiver-ca", "ca.crt", "/etc/pgwatch-tls/grpc-ca/ca.crt"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("mounted\n got  %v\n want %v", got, want)
	}

	params := connStrOf(t, monitor).Query()
	for param, path := range map[string]string{
		"sslrootcert": want[0].path, "sslcert": want[1].path, "sslkey": want[2].path,
	} {
		if params.Get(param) != path {
			t.Errorf("conn_str has %s=%q, the file is mounted at %q", param, params.Get(param), path)
		}
	}
	if params.Get("sslmode") != "verify-full" || len(params) != 4 {
		t.Errorf("conn_str parameters %v", params)
	}

	if sinks := sinkArgs(container.Args); !slices.Contains(sinks, "grpc://receiver.monitoring:9000?sslrootca="+want[3].path) {
		t.Errorf("sinks %q", sinks)
	}
	// What was there before is still there, and in front.
	if len(pod.Volumes) != 5 || pod.Volumes[0].ConfigMap == nil || pod.Volumes[1].EmptyDir == nil ||
		len(container.VolumeMounts) != 5 || container.VolumeMounts[0].MountPath != "/etc/pgwatch" {
		t.Errorf("volumes %+v\nmounts %+v", pod.Volumes, container.VolumeMounts)
	}
}

// A monitor without certificates is rendered exactly as before: nothing
// about it may change, or every collector would be restarted for a feature
// it does not use.
func TestTLS_NothingChangesWithoutCertificates(t *testing.T) {
	monitor := newTestMonitor()
	monitor.Spec.Sinks.GRPC = &pgpilotv1.GRPCSink{Enabled: true, Endpoint: "receiver.monitoring:9000"}
	monitor.Spec.Database.TLS = &pgpilotv1.DatabaseTLS{} // a block with nothing in it

	if got := connStrOf(t, monitor).String(); got != "postgresql://pg.example.com:5432/mydb?sslmode=require" {
		t.Errorf("conn_str %q", got)
	}
	dep := BuildDeployment(monitor, MergedMetrics{Preset: "basic"}, PodTemplateVersions{Config: "h"})
	pod := dep.Spec.Template.Spec
	if len(pod.Volumes) != 2 || len(pod.Containers[0].VolumeMounts) != 2 {
		t.Errorf("volumes %+v\nmounts %+v", pod.Volumes, pod.Containers[0].VolumeMounts)
	}
	if sinks := sinkArgs(pod.Containers[0].Args); !slices.Contains(sinks, "grpc://receiver.monitoring:9000") {
		t.Errorf("sinks %q", sinks)
	}
}

// Each certificate on its own: the others must not appear with it.
func TestTLS_EachCertificateOnItsOwn(t *testing.T) {
	cases := map[string]struct {
		tls        *pgpilotv1.DatabaseTLS
		wantParams []string
		wantVolume string
	}{
		"the certificate authority": {
			&pgpilotv1.DatabaseTLS{CASecretRef: &pgpilotv1.CASecretRef{Name: "pg-ca"}},
			[]string{"sslmode", "sslrootcert"}, "database-ca",
		},
		"the client certificate": {
			&pgpilotv1.DatabaseTLS{ClientCertSecretRef: &pgpilotv1.ClientCertSecretRef{Name: "pg-client"}},
			[]string{"sslcert", "sslkey", "sslmode"}, "database-client-cert",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			monitor := newTestMonitor()
			monitor.Spec.Database.TLS = tc.tls
			params := connStrOf(t, monitor).Query()
			got := make([]string, 0, len(params))
			for param := range params {
				got = append(got, param)
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.wantParams) {
				t.Errorf("conn_str parameters %v, want %v", got, tc.wantParams)
			}
			pod := BuildDeployment(monitor, MergedMetrics{}, PodTemplateVersions{Config: "h"}).Spec.Template.Spec
			if len(pod.Volumes) != 3 || pod.Volumes[2].Name != tc.wantVolume {
				t.Errorf("volumes %+v", pod.Volumes)
			}
		})
	}
}

// testPair returns a self-signed certificate and its key, the key as the PEM
// block of the given type ("PRIVATE KEY" for PKCS #8, "EC PRIVATE KEY").
func testPair(t *testing.T, keyType string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "unit test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	var keyDER []byte
	if keyType == "EC PRIVATE KEY" {
		keyDER, err = x509.MarshalECPrivateKey(key)
	} else {
		keyDER, err = x509.MarshalPKCS8PrivateKey(key)
	}
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: keyType, Bytes: keyDER})
}

// What a Secret holds is checked the way the collector loads it, and what
// is wrong with it is said without repeating any of it.
func TestMountedSecret_CheckContent(t *testing.T) {
	secrets := MountedSecrets(tlsMonitor())
	authority, clientCert := secrets[0], secrets[1]
	if authority.Name != "pg-ca" || clientCert.Name != "pg-client" || secrets[2].Name != "receiver-ca" {
		t.Fatalf("secrets %+v", secrets)
	}
	cert, key := testPair(t, "PRIVATE KEY")
	otherCert, ecKey := testPair(t, "EC PRIVATE KEY")
	ecCert := otherCert
	// What `openssl ecparam -genkey` writes: the parameters of the curve,
	// then the key. pgx takes the first block for the key.
	parameters := pem.EncodeToMemory(&pem.Block{Type: "EC PARAMETERS", Bytes: []byte{0x06, 0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07}})
	legacyEncrypted := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: []byte("ciphertext"),
		Headers: map[string]string{"Proc-Type": "4,ENCRYPTED", "DEK-Info": "AES-256-CBC,00"},
	})
	pkcs8Encrypted := pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: []byte("ciphertext")})
	cutShort := func(certPEM []byte) []byte {
		whole := mustDER(t, certPEM)
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: whole[:len(whole)-40]})
	}
	mislabelled := func(keyPEM []byte) []byte {
		return pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: mustDER(t, keyPEM)})
	}
	// The driver asks whether the header is there, not what it says.
	emptyHeader := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: []byte("ciphertext"), Headers: map[string]string{"DEK-Info": ""},
	})
	// Blocks of other kinds, as a Secret may hold them next to a
	// certificate. Their labels are content, and stay out of the message.
	marked := func(data []byte) []byte {
		return append(pem.EncodeToMemory(&pem.Block{Type: "MARKER OF THE SECRET", Bytes: []byte("x")}), data...)
	}

	t.Run("a certificate authority", func(t *testing.T) {
		for name, tc := range map[string]struct {
			content []byte
			want    string // "" for usable
		}{
			"one certificate":             {cert, ""},
			"a bundle":                    {append(append([]byte("# our roots\n"), cert...), otherCert...), ""},
			"a certificate after a key":   {append(append([]byte{}, key...), cert...), ""},
			"a key":                       {key, `key "ca.crt" holds no PEM certificate`},
			"text":                        {[]byte("see the wiki"), `key "ca.crt" holds no PEM certificate`},
			"PEM around something else":   {pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("no")}), `holds no PEM certificate`},
			"base64 of the PEM, not PEM":  {[]byte("LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0t"), `holds no PEM certificate`},
			"DER, which pgwatch does not": {mustDER(t, cert), `holds no PEM certificate`},
			// A block that says certificate and holds half of one: nothing
			// in it is an authority.
			"a certificate that is cut short":      {cutShort(cert), `key "ca.crt" holds no PEM certificate`},
			"one that is, after one that is whole": {append(append([]byte{}, cert...), cutShort(otherCert)...), ""},
		} {
			err := authority.CheckContent(map[string][]byte{"ca.crt": tc.content})
			if (err == nil) != (tc.want == "") || (err != nil && !strings.Contains(err.Error(), tc.want)) {
				t.Errorf("%s: got %v, want %q", name, err, tc.want)
			}
		}
		// The gRPC sink's authority is checked the same way.
		if err := secrets[2].CheckContent(map[string][]byte{"ca.crt": key}); err == nil {
			t.Error("a key passed for the authority of the gRPC sink")
		}
	})

	t.Run("a client certificate and its key", func(t *testing.T) {
		for name, tc := range map[string]struct {
			cert, key []byte
			want      string
		}{
			"PKCS #8":                        {cert, key, ""},
			"an EC key":                      {ecCert, ecKey, ""},
			"the certificate with its chain": {append(append([]byte{}, cert...), otherCert...), key, ""},
			"the key of another certificate": {otherCert, key, `keys "tls.crt" and "tls.key" do not hold a PEM certificate and, ` +
				`as the first PEM block, its private key: the key is not the one of the certificate`},
			"the wrong way round":          {key, cert, `key "tls.crt" holds no PEM certificate`},
			"two certificates":             {cert, cert, `do not hold a PEM certificate and, as the first PEM block, its private key`},
			"a header without a value":     {cert, emptyHeader, `key "tls.key" holds a private key under a passphrase`},
			"other blocks, and a key":      {marked(cert), key, ""},
			"other blocks, and no key":     {marked(cert), marked(key), `as the first PEM block, its private key`},
			"other blocks, no certificate": {marked(nil), key, `key "tls.crt" holds no PEM certificate`},
			"other blocks, another's key":  {marked(otherCert), key, `the key is not the one of the certificate`},
			// One value for both, as the collector reads it: the key is the
			// first block of the one, the certificate somewhere in the other.
			"one value, the key first":         {append(append([]byte{}, key...), cert...), append(append([]byte{}, key...), cert...), ""},
			"one value, the certificate first": {append(append([]byte{}, cert...), key...), append(append([]byte{}, cert...), key...), `as the first PEM block, its private key`},
			"a key that is no PEM":             {cert, []byte("secret"), `key "tls.key" holds no PEM block`},
			"curve parameters before the key":  {ecCert, append(append([]byte{}, parameters...), ecKey...), `as the first PEM block, its private key`},
			"a key under a passphrase":         {cert, legacyEncrypted, `key "tls.key" holds a private key under a passphrase`},
			"a PKCS #8 key under a passphrase": {cert, pkcs8Encrypted, `key "tls.key" holds a private key under a passphrase`},
			// The label alone decides nothing: the collector reads what is
			// under it.
			"a key in the clear under the label of one that is not": {cert, mislabelled(key), ""},
			"no certificate": {[]byte("soon"), key, `key "tls.crt" holds no PEM certificate`},
			// Not the key's fault, and not said to be.
			"a certificate that is cut short": {cutShort(cert), key, `key "tls.crt" holds a PEM certificate that cannot be read`},
		} {
			err := clientCert.CheckContent(map[string][]byte{"tls.crt": tc.cert, "tls.key": tc.key})
			if (err == nil) != (tc.want == "") || (err != nil && !strings.Contains(err.Error(), tc.want)) {
				t.Errorf("%s: got %v, want %q", name, err, tc.want)
			}
			if err != nil && (strings.Contains(err.Error(), "BEGIN") || strings.Contains(err.Error(), "ciphertext") ||
				strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "MARKER") ||
				strings.Contains(err.Error(), "tls:") || strings.Contains(err.Error(), "x509")) {
				t.Errorf("%s: the error repeats what the Secret holds: %v", name, err)
			}
		}
	})

	t.Run("the keys are the monitor's", func(t *testing.T) {
		monitor := tlsMonitor()
		monitor.Spec.Database.TLS.ClientCertSecretRef.CertKey = "client.pem"
		monitor.Spec.Database.TLS.ClientCertSecretRef.KeyKey = "client.key"
		pair := MountedSecrets(monitor)[1]
		if err := pair.CheckContent(map[string][]byte{"client.pem": cert, "client.key": key}); err != nil {
			t.Error(err)
		}
		err := pair.CheckContent(map[string][]byte{"client.pem": cert, "client.key": []byte("x")})
		if err == nil || !strings.Contains(err.Error(), `key "client.key" holds no PEM block`) {
			t.Errorf("got %v", err)
		}
	})
}

func mustDER(t *testing.T, certPEM []byte) []byte {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("no PEM")
	}
	return block.Bytes
}

// A certificate and its key can be one value of the Secret. The one key is
// then mounted twice, under both names the collector is given.
func TestBuildDeployment_OneKeyForTheCertificateAndItsKey(t *testing.T) {
	monitor := tlsMonitor()
	monitor.Spec.Database.TLS.CASecretRef = nil
	monitor.Spec.Sinks.GRPC = nil
	monitor.Spec.Database.TLS.ClientCertSecretRef = &pgpilotv1.ClientCertSecretRef{
		Name: "pg-client", CertKey: "client.pem", KeyKey: "client.pem",
	}
	if got := MountedSecrets(monitor); len(got) != 1 || !slices.Equal(got[0].Keys, []string{"client.pem", "client.pem"}) {
		t.Fatalf("mounted %+v", got)
	}
	pod := BuildDeployment(monitor, MergedMetrics{Preset: "basic"}, PodTemplateVersions{Config: "h"}).Spec.Template.Spec
	want := []corev1.KeyToPath{{Key: "client.pem", Path: "tls.crt"}, {Key: "client.pem", Path: "tls.key"}}
	if len(pod.Volumes) != 3 || pod.Volumes[2].Secret == nil || !slices.Equal(pod.Volumes[2].Secret.Items, want) {
		t.Errorf("volumes %+v", pod.Volumes)
	}
	params := connStrOf(t, monitor).Query()
	if params.Get("sslcert") != "/etc/pgwatch-tls/database-client-cert/tls.crt" ||
		params.Get("sslkey") != "/etc/pgwatch-tls/database-client-cert/tls.key" {
		t.Errorf("conn_str parameters %v", params)
	}
}

// pgwatch takes the address of the sink apart as a URL and connects with TLS
// only when it finds the parameter sslrootca. Whatever the endpoint carries
// of its own, the authority has to arrive as that parameter.
func TestGRPCSinkAddress(t *testing.T) {
	const authority = "/etc/pgwatch-tls/grpc-ca/ca.crt"
	cases := map[string]struct {
		endpoint string
		want     string // with an authority
	}{
		"host and port":              {"receiver.monitoring:9000", "grpc://receiver.monitoring:9000?sslrootca=" + authority},
		"a user and a password":      {"user:pass@receiver:9000", "grpc://user:pass@receiver:9000?sslrootca=" + authority},
		"a trailing slash":           {"receiver:9000/", "grpc://receiver:9000/?sslrootca=" + authority},
		"an IPv6 address":            {"[fd00::1]:9000", "grpc://[fd00::1]:9000?sslrootca=" + authority},
		"a parameter of its own":     {"receiver:9000?timeout=5", "grpc://receiver:9000?timeout=5&sslrootca=" + authority},
		"an empty query":             {"receiver:9000?", "grpc://receiver:9000?sslrootca=" + authority},
		"a fragment":                 {"receiver:9000#prod", "grpc://receiver:9000?sslrootca=" + authority},
		"a query and a fragment":     {"receiver:9000?a=1#prod", "grpc://receiver:9000?a=1&sslrootca=" + authority},
		"an authority named by hand": {"receiver:9000?sslrootca=/etc/ssl/cert.pem", "grpc://receiver:9000?sslrootca=" + authority},
		"by hand, among others":      {"receiver:9000?sslrootca=/x&b=2&sslrootca=/y", "grpc://receiver:9000?b=2&sslrootca=" + authority},
		// What pgwatch cannot take apart either: it ends over it, with or
		// without the authority.
		"a port that is none": {"receiver:metrics", "grpc://receiver:metrics?sslrootca=" + authority},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			monitor := tlsMonitor()
			monitor.Spec.Sinks.GRPC.Endpoint = tc.endpoint
			got := grpcSinkAddress(monitor)
			if got != tc.want {
				t.Errorf("got  %s\nwant %s", got, tc.want)
			}
			// As pgwatch reads it (internal/sinks/rpc.go).
			if parsed, err := url.Parse(got); err == nil {
				params, err := url.ParseQuery(parsed.RawQuery)
				if err != nil || len(params["sslrootca"]) != 1 || params["sslrootca"][0] != authority {
					t.Errorf("pgwatch would find sslrootca=%q (%v) in %s", params["sslrootca"], err, got)
				}
			}

			// Without an authority the address is the endpoint, to the
			// letter: nothing about such a monitor may change.
			monitor.Spec.Sinks.GRPC.TLS = nil
			if got := grpcSinkAddress(monitor); got != "grpc://"+tc.endpoint {
				t.Errorf("without an authority: %s", got)
			}
		})
	}
}

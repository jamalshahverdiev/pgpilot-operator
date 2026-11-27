//go:build collector

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

package collector

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
	"github.com/jamalshahverdiev/pgpilot-operator/internal/builder"
)

// authority is a certificate authority made for one test run.
type authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	// pem is the certificate as a Secret would hold it under ca.crt.
	pem []byte
}

func newAuthority(name string) (*authority, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &authority{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}, nil
}

// issue signs a certificate for a server (hosts) or for a client (the common
// name is the database user), and returns it and its key as PEM.
func (a *authority) issue(commonName string, hosts ...string) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		return nil, nil, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     hosts,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, a.cert, &key.PublicKey, a.key)
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

// certUser logs in with a client certificate and nothing else.
const certUser = "pgwatch_cert"

// postgresAuthority turns TLS on in the PostgreSQL container, once, and
// returns the authority that signed its certificate. Connections without TLS
// go on working: the other tests share the server.
var postgresAuthority = sync.OnceValues(func() (*authority, error) {
	ca, err := newAuthority("collector test database CA")
	if err != nil {
		return nil, err
	}
	serverCert, serverKey, err := ca.issue(postgresBox, postgresBox)
	if err != nil {
		return nil, err
	}
	dataDir, err := psql("SHOW data_directory")
	if err != nil {
		return nil, fmt.Errorf("find the data directory: %w\n%s", err, dataDir)
	}
	dataDir = strings.TrimSpace(dataDir)

	local, err := os.MkdirTemp("", "collector-test-tls-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(local) }()
	for name, content := range map[string][]byte{"server.crt": serverCert, "server.key": serverKey, "root.crt": ca.pem} {
		path := filepath.Join(local, name)
		if err := os.WriteFile(path, content, 0o600); err != nil {
			return nil, err
		}
		if out, err := docker("cp", path, postgresBox+":"+dataDir+"/"+name); err != nil {
			return nil, fmt.Errorf("copy %s into the server: %w\n%s", name, err, out)
		}
	}
	// The server refuses a key that others can read, and copied files
	// belong to root. A client certificate is asked for, and is all it
	// takes, for certUser alone; the line goes first because the first
	// match decides.
	setup := fmt.Sprintf(`set -e
cd %q
chown postgres:postgres server.crt server.key root.crt
chmod 600 server.key
sed -i '1i hostssl all %s all cert' pg_hba.conf`, dataDir, certUser)
	if out, err := docker("exec", "--user", "root", postgresBox, "sh", "-c", setup); err != nil {
		return nil, fmt.Errorf("prepare the server's certificate: %w\n%s", err, out)
	}
	for _, statement := range []string{
		"ALTER SYSTEM SET ssl = on",
		"ALTER SYSTEM SET ssl_ca_file = 'root.crt'",
		fmt.Sprintf("CREATE ROLE %s LOGIN", certUser),
		fmt.Sprintf("GRANT pg_monitor TO %s", certUser),
		"SELECT pg_reload_conf()",
	} {
		if out, err := psql(statement); err != nil {
			return nil, fmt.Errorf("%s: %w\n%s", statement, err, out)
		}
	}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(500 * time.Millisecond) {
		if out, err := psql("SHOW ssl"); err == nil && strings.TrimSpace(out) == "on" {
			return ca, nil
		}
		if time.Now().After(deadline) {
			logs, _ := docker("logs", "--tail", "30", postgresBox)
			return nil, fmt.Errorf("the server did not turn TLS on:\n%s", logs)
		}
	}
})

// encryptedSessions counts the sessions of a database user that use TLS.
func encryptedSessions(t *testing.T, user string) int {
	t.Helper()
	out, err := psql(fmt.Sprintf(
		"SELECT count(*) FROM pg_stat_ssl s JOIN pg_stat_activity a USING (pid) WHERE a.usename = '%s' AND s.ssl", user))
	if err != nil {
		t.Fatalf("pg_stat_ssl: %v\n%s", err, out)
	}
	var count int
	if _, err := fmt.Sscan(strings.TrimSpace(out), &count); err != nil {
		t.Fatalf("pg_stat_ssl answered %q", out)
	}
	return count
}

// The certificate authority of spec.database.tls is what sslmode verify-full
// checks the server against. Without it pgwatch has the authorities of its
// image, which never signed the certificate of a database of one's own.
func TestDatabaseTLS(t *testing.T) {
	ca, err := postgresAuthority()
	if err != nil {
		t.Fatal(err)
	}

	verifyFull := func(name string) *pgpilotv1.PgpilotMonitor {
		monitor := newMonitor(name, pgpilotv1.MetricsSpec{Preset: "basic"})
		monitor.Spec.Database.SSLMode = pgpilotv1.SSLModeVerifyFull
		monitor.Spec.Database.TLS = &pgpilotv1.DatabaseTLS{CASecretRef: &pgpilotv1.CASecretRef{Name: "pg-ca"}}
		return monitor
	}
	withCA := func(certificate []byte, key string) secrets {
		held := defaultSecrets()
		held["pg-ca"] = map[string]string{key: string(certificate)}
		return held
	}
	// refused waits for the collector to say why it cannot connect.
	refused := func(t *testing.T, c *collector, because string) {
		t.Helper()
		for deadline := time.Now().Add(time.Minute); ; time.Sleep(time.Second) {
			logs := c.logs()
			if strings.Contains(logs, "Connect OK") {
				t.Fatalf("the collector connected:\n%s", logs)
			}
			if strings.Contains(logs, because) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("the collector did not report %q within a minute:\n%s", because, logs)
			}
		}
	}

	t.Run("the server is checked against the authority of the Secret", func(t *testing.T) {
		t.Parallel()
		monitor := verifyFull("tls-verified")
		c := startCollector(t, monitor, nil, withCA(ca.pem, "ca.crt"))
		c.eventually("first samples over TLS", up(monitor.Name))
		if encryptedSessions(t, monitoringUser) == 0 {
			t.Errorf("no session of %s uses TLS", monitoringUser)
		}
		c.expectCleanLog()
	})

	t.Run("under a key of the monitor's choosing", func(t *testing.T) {
		t.Parallel()
		monitor := verifyFull("tls-own-key")
		monitor.Spec.Database.TLS.CASecretRef.Key = "root.pem"
		c := startCollector(t, monitor, nil, withCA(ca.pem, "root.pem"))
		c.eventually("first samples over TLS", up(monitor.Name))
	})

	t.Run("an authority that did not sign the server's certificate is no good", func(t *testing.T) {
		t.Parallel()
		other, err := newAuthority("somebody else")
		if err != nil {
			t.Fatal(err)
		}
		c := startCollector(t, verifyFull("tls-wrong-ca"), nil, withCA(other.pem, "ca.crt"))
		refused(t, c, "certificate signed by unknown authority")
	})

	// The reason the field exists: this is what a monitor with verify-full
	// got before it could name an authority.
	t.Run("without the Secret the image's authorities decide, and do not know this one", func(t *testing.T) {
		t.Parallel()
		monitor := verifyFull("tls-no-ca")
		monitor.Spec.Database.TLS = nil
		c := startCollector(t, monitor, nil, defaultSecrets())
		refused(t, c, "certificate signed by unknown authority")
	})

	t.Run("a client certificate logs in where the server asks for one", func(t *testing.T) {
		t.Parallel()
		clientCert, clientKey, err := ca.issue(certUser)
		if err != nil {
			t.Fatal(err)
		}
		monitor := verifyFull("tls-client-cert")
		monitor.Spec.Database.TLS.ClientCertSecretRef = &pgpilotv1.ClientCertSecretRef{Name: "pg-client"}
		held := withCA(ca.pem, "ca.crt")
		// The password is not the user's: the certificate is what the
		// server goes by.
		held[credentialsSecret] = map[string]string{"username": certUser, "password": "not-asked-for"}
		held["pg-client"] = map[string]string{"tls.crt": string(clientCert), "tls.key": string(clientKey)}
		c := startCollector(t, monitor, nil, held)
		c.eventually("first samples with a client certificate", up(monitor.Name))
		if encryptedSessions(t, certUser) == 0 {
			t.Errorf("no session of %s uses TLS", certUser)
		}
		c.expectCleanLog()
	})

	t.Run("without the client certificate that user is turned away", func(t *testing.T) {
		t.Parallel()
		held := withCA(ca.pem, "ca.crt")
		held[credentialsSecret] = map[string]string{"username": certUser, "password": "not-asked-for"}
		c := startCollector(t, verifyFull("tls-no-client-cert"), nil, held)
		refused(t, c, "certificate")
	})

	// What sslmode makes of the authority. With require the server is
	// checked against it, as libpq documents; with prefer nothing is, and a
	// monitor that names an authority there would look protected and not
	// be. The API refuses that pair, and this is why.
	t.Run("with sslmode require the authority is checked", func(t *testing.T) {
		t.Parallel()
		other, err := newAuthority("somebody else")
		if err != nil {
			t.Fatal(err)
		}
		monitor := verifyFull("tls-require-wrong-ca")
		monitor.Spec.Database.SSLMode = pgpilotv1.SSLModeRequire
		c := startCollector(t, monitor, nil, withCA(other.pem, "ca.crt"))
		refused(t, c, "certificate signed by unknown authority")
	})

	t.Run("with sslmode prefer nothing checks the server, whatever the authority", func(t *testing.T) {
		t.Parallel()
		other, err := newAuthority("somebody else")
		if err != nil {
			t.Fatal(err)
		}
		monitor := verifyFull("tls-prefer-wrong-ca")
		monitor.Spec.Database.SSLMode = pgpilotv1.SSLModePrefer
		c := startCollector(t, monitor, nil, withCA(other.pem, "ca.crt"))
		c.eventually("first samples from a server nobody checked", up(monitor.Name))
	})
}

// The operator refuses a Secret whose content the collector could not load,
// before a pod is made. Its check (builder.MountedSecret.CheckContent) is a
// copy of what the collector does with the files, and a copy can be wrong in
// both directions: it could let through what the collector then fails over,
// where only its log says so, or refuse what would have worked. Each case is
// therefore put to both, and they have to agree.
func TestTheOperatorsCheckOfCertificatesAgreesWithTheCollector(t *testing.T) {
	ca, err := postgresAuthority()
	if err != nil {
		t.Fatal(err)
	}
	other, err := newAuthority("somebody else")
	if err != nil {
		t.Fatal(err)
	}
	cert, key, err := ca.issue(certUser)
	if err != nil {
		t.Fatal(err)
	}
	otherCert, _, err := ca.issue(certUser)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der(key))
	if err != nil {
		t.Fatal(err)
	}
	sec1, err := x509.MarshalECPrivateKey(parsed.(*ecdsa.PrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	ecKey := block("EC PRIVATE KEY", sec1, nil)
	parameters := curveParameters
	if _, err := tls.X509KeyPair(cert, join(parameters, ecKey)); err != nil {
		t.Fatalf("a key after its parameters is one for crypto/tls, or that case shows nothing: %v", err)
	}

	// connects waits for the collector to say whether it got into the
	// database.
	connects := func(t *testing.T, c *collector) bool {
		t.Helper()
		for deadline := time.Now().Add(firstSamples); ; time.Sleep(time.Second) {
			logs := c.logs()
			switch {
			case strings.Contains(logs, "Connect OK"):
				return true
			case strings.Contains(logs, "could not init connection"), !c.running():
				return false
			case time.Now().After(deadline):
				t.Fatalf("the collector neither connected nor said why not:\n%s", logs)
			}
		}
	}
	t.Run("a client certificate and its key", func(t *testing.T) {
		cases := []struct {
			name      string
			cert, key []byte
			oneValue  bool // the Secret has one key, named for both
			want      bool
		}{
			{"a PKCS #8 key", cert, key, false, true},
			{"an EC key", cert, ecKey, false, true},
			{"the certificate with a chain", join(cert, ca.pem), key, false, true},
			{"other blocks around the certificate", join(parameters, cert), key, false, true},
			{"one value, the key first", join(key, cert), nil, true, true},
			{"one value, the certificate first", join(cert, key), nil, true, false},
			{"the key of another certificate", otherCert, key, false, false},
			{"the wrong way round", key, cert, false, false},
			{"no certificate", []byte("soon"), key, false, false},
			{"a certificate that is cut short", block("CERTIFICATE", der(cert)[:len(der(cert))-40], nil), key, false, false},
			{"a key that is no PEM", cert, der(key), false, false},
			{"a key after the parameters of its curve", cert, join(parameters, ecKey), false, false},
			{"a key under a passphrase", cert, block("EC PRIVATE KEY", sec1, map[string]string{
				"Proc-Type": "4,ENCRYPTED", "DEK-Info": "AES-256-CBC,00112233445566778899AABBCCDDEEFF",
			}), false, false},
			{"a header that says so without a value", cert,
				block("EC PRIVATE KEY", sec1, map[string]string{"DEK-Info": ""}), false, false},
			{"a PKCS #8 key under a passphrase", cert, block("ENCRYPTED PRIVATE KEY", []byte("ciphertext"), nil), false, false},
			// The label alone decides nothing: the collector reads what is
			// under it.
			{"a key in the clear under that label", cert, block("ENCRYPTED PRIVATE KEY", der(key), nil), false, true},
		}
		for i, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				monitor := newMonitor(fmt.Sprintf("tls-pair-%d", i), pgpilotv1.MetricsSpec{Preset: "basic"})
				monitor.Spec.Database.SSLMode = pgpilotv1.SSLModeVerifyFull
				monitor.Spec.Database.TLS = &pgpilotv1.DatabaseTLS{
					CASecretRef:         &pgpilotv1.CASecretRef{Name: "pg-ca"},
					ClientCertSecretRef: &pgpilotv1.ClientCertSecretRef{Name: "pg-client"},
				}
				data := map[string][]byte{"tls.crt": tc.cert, "tls.key": tc.key}
				if tc.oneValue {
					monitor.Spec.Database.TLS.ClientCertSecretRef.CertKey = "client.pem"
					monitor.Spec.Database.TLS.ClientCertSecretRef.KeyKey = "client.pem"
					data = map[string][]byte{"client.pem": tc.cert}
				}
				verdict := builder.MountedSecrets(monitor)[1].CheckContent(data)
				if (verdict == nil) != tc.want {
					t.Errorf("the operator says %v", verdict)
				}

				held := defaultSecrets()
				held["pg-ca"] = map[string]string{"ca.crt": string(ca.pem)}
				// The password is not the user's: with a certificate the
				// server asks for none, and without one it turns the user
				// away whatever the password.
				held[credentialsSecret] = map[string]string{"username": certUser, "password": "not-asked-for"}
				held["pg-client"] = map[string]string{}
				for name, value := range data {
					held["pg-client"][name] = string(value)
				}
				c := startCollector(t, monitor, nil, held)
				agree(t, verdict, connects(t, c), c)
			})
		}
	})

	t.Run("the authority of the database", func(t *testing.T) {
		cases := []struct {
			name    string
			content []byte
			want    bool
		}{
			{"one certificate", ca.pem, true},
			{"a bundle, with a line of text", join([]byte("# our roots\n"), other.pem, ca.pem), true},
			{"a certificate after other blocks", join(parameters, ca.pem), true},
			{"a certificate that is cut short", block("CERTIFICATE", der(ca.pem)[:len(der(ca.pem))-40], nil), false},
			{"a key", key, false},
			{"text", []byte("see the wiki"), false},
			{"the PEM, encoded once more", []byte(base64.StdEncoding.EncodeToString(ca.pem)), false},
			{"DER", der(ca.pem), false},
		}
		for i, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				monitor := newMonitor(fmt.Sprintf("tls-ca-%d", i), pgpilotv1.MetricsSpec{Preset: "basic"})
				monitor.Spec.Database.SSLMode = pgpilotv1.SSLModeVerifyFull
				monitor.Spec.Database.TLS = &pgpilotv1.DatabaseTLS{CASecretRef: &pgpilotv1.CASecretRef{Name: "pg-ca"}}
				verdict := builder.MountedSecrets(monitor)[0].CheckContent(map[string][]byte{"ca.crt": tc.content})
				if (verdict == nil) != tc.want {
					t.Errorf("the operator says %v", verdict)
				}
				held := defaultSecrets()
				held["pg-ca"] = map[string]string{"ca.crt": string(tc.content)}
				c := startCollector(t, monitor, nil, held)
				agree(t, verdict, connects(t, c), c)
			})
		}
	})
}

// agree fails the test where the operator's verdict on a Secret and what the
// collector made of it differ.
func agree(t *testing.T, verdict error, works bool, c *collector) {
	t.Helper()
	switch {
	case verdict == nil && !works:
		t.Errorf("the operator lets this through and the collector cannot use it:\n%s", c.logs())
	case verdict != nil && works:
		t.Errorf("the collector works with what the operator refuses: %v", verdict)
	case verdict != nil:
		t.Logf("refused by both; the operator says: %v", verdict)
	}
}

func join(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func block(blockType string, content []byte, headers map[string]string) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: content, Headers: headers})
}

func der(pemBytes []byte) []byte {
	decoded, _ := pem.Decode(pemBytes)
	return decoded.Bytes
}

// What `openssl ecparam -genkey` writes in front of a key: the object
// identifier of the curve, here P-256.
var curveParameters = block("EC PARAMETERS", []byte{0x06, 0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07}, nil)

// The gRPC sink has no way to go on without its receiver: the collector ends
// over an authority it cannot load.
func TestTheOperatorsCheckOfTheGRPCAuthorityAgreesWithTheCollector(t *testing.T) {
	receiverCA, err := newAuthority("collector test receiver CA")
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := receiverCA.issue("somebody")
	if err != nil {
		t.Fatal(err)
	}
	parameters := curveParameters
	cases := []struct {
		name    string
		content []byte
		want    bool
	}{
		{"one certificate", receiverCA.pem, true},
		{"a certificate after other blocks", join(parameters, receiverCA.pem), true},
		{"a key", key, false},
		{"the PEM, encoded once more", []byte(base64.StdEncoding.EncodeToString(receiverCA.pem)), false},
		{"DER", der(receiverCA.pem), false},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sink := tlsReceiver(t, receiverCA)
			name := fmt.Sprintf("tls-grpc-ca-%d", i)
			monitor := grpcOnly(name, sink.endpoint)
			monitor.Spec.Sinks.GRPC.TLS = &pgpilotv1.GRPCTLS{CASecretRef: "receiver-ca"}
			verdict := builder.MountedSecrets(monitor)[0].CheckContent(map[string][]byte{"ca.crt": tc.content})
			if (verdict == nil) != tc.want {
				t.Errorf("the operator says %v", verdict)
			}
			held := defaultSecrets()
			held["receiver-ca"] = map[string]string{"ca.crt": string(tc.content)}
			c := startCollector(t, monitor, nil, held)

			delivered := func() bool {
				for _, message := range sink.measurements() {
					if bytes.Contains(message, []byte(name)) {
						return true
					}
				}
				return false
			}
			works := false
			for deadline := time.Now().Add(firstSamples); ; time.Sleep(time.Second) {
				if works = delivered(); works || !c.running() {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the collector runs and delivers nothing:\n%s", c.logs())
				}
			}
			agree(t, verdict, works, c)
		})
	}

}

// tlsReceiver is a receiver that only speaks TLS, with a certificate for the
// name the collector knows it by.
func tlsReceiver(t *testing.T, ca *authority) *receiver {
	t.Helper()
	certPEM, keyPEM, err := ca.issue("collector test receiver", receiverHost)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return startReceiver(t, grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12,
	})))
}

// The certificate authority of spec.sinks.grpc.tls is what turns TLS on for
// the gRPC sink. The field was accepted and ignored: a receiver that only
// speaks TLS could not be used at all.
func TestGRPCSinkWithTLS(t *testing.T) {
	ca, err := newAuthority("collector test receiver CA")
	if err != nil {
		t.Fatal(err)
	}
	withTLS := func(name, endpoint string) *pgpilotv1.PgpilotMonitor {
		monitor := grpcOnly(name, endpoint)
		monitor.Spec.Sinks.GRPC.TLS = &pgpilotv1.GRPCTLS{CASecretRef: "receiver-ca"}
		return monitor
	}
	holding := func(certificate []byte) secrets {
		held := defaultSecrets()
		held["receiver-ca"] = map[string]string{"ca.crt": string(certificate)}
		return held
	}

	t.Run("measurements arrive at a receiver that only speaks TLS", func(t *testing.T) {
		t.Parallel()
		sink := tlsReceiver(t, ca)
		c := startCollector(t, withTLS("grpc-tls", sink.endpoint), nil, holding(ca.pem))
		delivered := func() bool {
			for _, message := range sink.measurements() {
				if bytes.Contains(message, []byte("grpc-tls")) && bytes.Contains(message, []byte("instance_up")) {
					return true
				}
			}
			return false
		}
		for deadline := time.Now().Add(firstSamples); !delivered(); time.Sleep(time.Second) {
			if !c.running() {
				t.Fatalf("the collector exited:\n%s", c.logs())
			}
			if time.Now().After(deadline) {
				t.Fatalf("no measurement arrived within %s; the receiver saw %v", firstSamples, sink.methods())
			}
		}
		c.expectCleanLog()
	})

	// Without the field the collector speaks plaintext, which such a
	// receiver does not answer: this is what every monitor got.
	t.Run("without the Secret the collector cannot use that receiver", func(t *testing.T) {
		t.Parallel()
		sink := tlsReceiver(t, ca)
		c := startCollector(t, grpcOnly("grpc-tls-plain", sink.endpoint), nil, defaultSecrets())
		for deadline := time.Now().Add(time.Minute); c.running(); time.Sleep(500 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("the collector still runs after a minute:\n%s", c.logs())
			}
		}
		if got := sink.measurements(); len(got) != 0 {
			t.Errorf("%d measurements arrived in plaintext at a receiver that only speaks TLS", len(got))
		}
	})

	t.Run("an authority that did not sign the receiver's certificate is no good", func(t *testing.T) {
		t.Parallel()
		other, err := newAuthority("somebody else")
		if err != nil {
			t.Fatal(err)
		}
		sink := tlsReceiver(t, ca)
		c := startCollector(t, withTLS("grpc-tls-wrong-ca", sink.endpoint), nil, holding(other.pem))
		for deadline := time.Now().Add(time.Minute); c.running(); time.Sleep(500 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("the collector still runs after a minute:\n%s", c.logs())
			}
		}
		if got := sink.measurements(); len(got) != 0 {
			t.Errorf("%d measurements arrived at a receiver the collector had no reason to trust", len(got))
		}
	})
}

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
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
)

// Where the certificates are in the collector pod. One directory for each
// Secret volume, and one fixed file name for each thing, whatever the key in
// the Secret is called: the connection string and the sink address then do
// not depend on it.
const (
	tlsMountRoot = "/etc/pgwatch-tls"

	databaseCAVolume         = "database-ca"
	databaseClientCertVolume = "database-client-cert"
	grpcCAVolume             = "grpc-ca"

	caFile         = "ca.crt"
	clientCertFile = "tls.crt"
	clientKeyFile  = "tls.key"

	defaultCAKey         = "ca.crt"
	defaultClientCertKey = "tls.crt"
	defaultClientKeyKey  = "tls.key"
)

// MountedSecret is a Secret the collector pod mounts certificates from: the
// field of the monitor that names it, and the keys that have to be there
// for the pod to start.
type MountedSecret struct {
	// Field is the path of the field in the monitor's spec.
	Field string
	// Name of the Secret, in the monitor's namespace.
	Name string
	// Keys the pod asks for. A Secret volume that names a key the Secret
	// does not have keeps the pod from starting.
	Keys []string

	volume string
	// files are the names the Keys are mounted under, in their order. Two
	// names can be for one key: a certificate and its key in one value.
	files []string
	// clientCertificate says that Keys are a certificate and its private
	// key, in that order; otherwise the one key is a certificate authority.
	clientCertificate bool
}

// CheckContent reports why the collector could not use what the Secret
// holds under Keys, all of which are present: nil, or an error that says
// what is wrong without repeating any of the content. The errors of the
// libraries are not passed on for that reason: they name the kinds of PEM
// blocks they found, and whoever may write a monitor can point it at any
// key of any Secret in the namespace.
//
// A Secret with the right keys and the wrong content starts a pod that then
// fails where nobody looks: pgwatch cannot connect to the database, or exits
// over its gRPC sink.
func (s MountedSecret) CheckContent(data map[string][]byte) error {
	if s.clientCertificate {
		return checkClientCertificate(s.Keys[0], data[s.Keys[0]], s.Keys[1], data[s.Keys[1]])
	}
	if !x509.NewCertPool().AppendCertsFromPEM(data[s.Keys[0]]) {
		return errors.New("key \"" + s.Keys[0] + "\" holds no PEM certificate")
	}
	return nil
}

// MountedSecrets lists the Secrets the collector pod of a monitor mounts
// certificates from, in a fixed order: the database's certificate authority,
// the client certificate, the gRPC sink's certificate authority.
func MountedSecrets(monitor *pgpilotv1.PgpilotMonitor) []MountedSecret {
	var out []MountedSecret
	if certs := monitor.Spec.Database.TLS; certs != nil {
		if ref := certs.CASecretRef; ref != nil && ref.Name != "" {
			key := orDefault(ref.Key, defaultCAKey)
			out = append(out, MountedSecret{
				Field: "spec.database.tls.caSecretRef", Name: ref.Name, Keys: []string{key},
				volume: databaseCAVolume, files: []string{caFile},
			})
		}
		if ref := certs.ClientCertSecretRef; ref != nil && ref.Name != "" {
			cert, key := orDefault(ref.CertKey, defaultClientCertKey), orDefault(ref.KeyKey, defaultClientKeyKey)
			out = append(out, MountedSecret{
				Field: "spec.database.tls.clientCertSecretRef", Name: ref.Name, Keys: []string{cert, key},
				volume: databaseClientCertVolume, files: []string{clientCertFile, clientKeyFile},
				clientCertificate: true,
			})
		}
	}
	if name := grpcCASecret(monitor); name != "" {
		out = append(out, MountedSecret{
			Field: "spec.sinks.grpc.tls.caSecretRef", Name: name, Keys: []string{defaultCAKey},
			volume: grpcCAVolume, files: []string{caFile},
		})
	}
	return out
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// grpcCASecret returns the Secret with the certificate authority of the gRPC
// sink, or "" when the sink is not in use or connects without TLS.
func grpcCASecret(monitor *pgpilotv1.PgpilotMonitor) string {
	if !GRPCSinkEnabled(monitor) || monitor.Spec.Sinks.GRPC.TLS == nil {
		return ""
	}
	return monitor.Spec.Sinks.GRPC.TLS.CASecretRef
}

// checkClientCertificate loads a certificate and its key the way the
// collector's PostgreSQL driver does (pgx, pgconn.configTLS): the key is the
// first PEM block of its file and nothing after it, so a key that follows a
// block of curve parameters is not found, and a key under a passphrase cannot
// be opened, because nothing hands the collector one. The certificate may
// have other blocks around it, which is what lets one value hold both: the
// key first, then the certificate.
func checkClientCertificate(certKey string, cert []byte, keyKey string, key []byte) error {
	block, _ := pem.Decode(key)
	if block == nil {
		return fmt.Errorf("key %q holds no PEM block", keyKey)
	}
	underPassphrase := fmt.Errorf(
		"key %q holds a private key under a passphrase, which the collector cannot be given", keyKey)
	// What x509.IsEncryptedPEMBlock, which the driver asks, goes by: that
	// the header is there, whatever it says.
	if _, legacy := block.Headers["DEK-Info"]; legacy {
		return underPassphrase
	}
	first := firstCertificate(cert)
	if first == nil {
		return errors.New("key \"" + certKey + "\" holds no PEM certificate")
	}
	// Told apart from a key that is wrong: the message below sends its
	// reader to the key.
	if _, err := x509.ParseCertificate(first); err != nil {
		return errors.New("key \"" + certKey + "\" holds a PEM certificate that cannot be read")
	}
	if _, err := tls.X509KeyPair(cert, pem.EncodeToMemory(block)); err != nil {
		// The PKCS #8 form of a key under a passphrase has no header. The
		// driver does not know it and fails to read it as a key; the label
		// says why, where reading it has failed, and not before: a key in
		// the clear under that label is one the collector takes.
		if block.Type == "ENCRYPTED PRIVATE KEY" {
			return underPassphrase
		}
		pair := fmt.Sprintf("keys %q and %q do not hold a PEM certificate and, as the first PEM block, its private key",
			certKey, keyKey)
		// The one cause that is worth telling apart, by the two messages
		// crypto/tls has had for it since Go 1.
		if strings.Contains(err.Error(), "does not match public key") {
			return errors.New(pair + ": the key is not the one of the certificate")
		}
		return errors.New(pair)
	}
	return nil
}

// firstCertificate returns what the first CERTIFICATE block holds, the one
// the collector presents as its own, or nil when there is none.
func firstCertificate(data []byte) []byte {
	for {
		var block *pem.Block
		if block, data = pem.Decode(data); block == nil {
			return nil
		}
		if block.Type == "CERTIFICATE" {
			return block.Bytes
		}
	}
}

// tlsVolumes returns the volumes and mounts for MountedSecrets.
//
// The files are readable by everyone in the container (0444). The files of
// a Secret volume belong to root, the collector runs as UID 65534, and the
// pod has no fsGroup that would hand them to it; nothing else runs there. A
// key that only its owner may read would need that fsGroup, on every pod
// with certificates, and an OpenShift namespace assigns its own.
func tlsVolumes(monitor *pgpilotv1.PgpilotMonitor) ([]corev1.Volume, []corev1.VolumeMount) {
	secrets := MountedSecrets(monitor)
	volumes := make([]corev1.Volume, 0, len(secrets))
	mounts := make([]corev1.VolumeMount, 0, len(secrets))
	for _, secret := range secrets {
		items := make([]corev1.KeyToPath, 0, len(secret.Keys))
		for i, key := range secret.Keys {
			items = append(items, corev1.KeyToPath{Key: key, Path: secret.files[i]})
		}
		volumes = append(volumes, corev1.Volume{
			Name: secret.volume,
			Secret: &corev1.SecretVolumeSource{
				SecretName:  secret.Name,
				Items:       items,
				DefaultMode: ptr.To[int32](0o444),
			},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name: secret.volume, MountPath: tlsMountRoot + "/" + secret.volume, ReadOnly: true,
		})
	}
	return volumes, mounts
}

// databaseTLSParams adds to the parameters of the connection string where
// the collector finds the certificates of MountedSecrets.
func databaseTLSParams(db pgpilotv1.DatabaseRef, params url.Values) {
	if db.TLS == nil {
		return
	}
	if ref := db.TLS.CASecretRef; ref != nil && ref.Name != "" {
		params.Set("sslrootcert", tlsMountRoot+"/"+databaseCAVolume+"/"+caFile)
	}
	if ref := db.TLS.ClientCertSecretRef; ref != nil && ref.Name != "" {
		params.Set("sslcert", tlsMountRoot+"/"+databaseClientCertVolume+"/"+clientCertFile)
		params.Set("sslkey", tlsMountRoot+"/"+databaseClientCertVolume+"/"+clientKeyFile)
	}
}

// grpcSinkAddress is the address pgwatch is given for the gRPC sink. pgwatch
// connects with TLS when the address names a certificate authority, and in
// plaintext otherwise.
//
// pgwatch takes the address apart as a URL and looks for the parameter
// sslrootca. An endpoint may have parameters of its own, or a fragment: put
// behind them, the authority would be part of something else, and pgwatch
// would connect in plaintext without a word. Without an authority the
// address is the endpoint as it is, as it always was.
func grpcSinkAddress(monitor *pgpilotv1.PgpilotMonitor) string {
	address := "grpc://" + monitor.Spec.Sinks.GRPC.Endpoint
	if grpcCASecret(monitor) == "" {
		return address
	}
	authority := "sslrootca=" + tlsMountRoot + "/" + grpcCAVolume + "/" + caFile
	parsed, err := url.Parse(address)
	if err != nil {
		// pgwatch cannot take it apart either, and ends over it.
		return address + "?" + authority
	}
	// One that is already there, from when this was the only way to name
	// an authority, gives way to the Secret.
	rest := parsed.Query()
	rest.Del("sslrootca")
	parsed.RawQuery = authority
	if len(rest) > 0 {
		parsed.RawQuery = rest.Encode() + "&" + authority
	}
	parsed.Fragment, parsed.RawFragment = "", ""
	return parsed.String()
}

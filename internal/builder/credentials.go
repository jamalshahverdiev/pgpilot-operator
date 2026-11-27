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
	corev1 "k8s.io/api/core/v1"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
)

// The keys of the generated credentials Secret.
const (
	generatedUsernameKey = "username"
	generatedPasswordKey = "password"
)

// CredentialsSecretName is the name of the Secret the operator generates for
// a monitor whose credentials are written into its spec.
func CredentialsSecretName(monitorName string) string {
	return ResourceName(monitorName) + "-credentials"
}

// HasInlineCredentials reports whether the monitor carries its credentials
// in its own spec. Only a username together with a password counts (the CRD
// asks for both, or for a Secret).
func HasInlineCredentials(monitor *pgpilotv1.PgpilotMonitor) bool {
	return monitor.Spec.Database.Username != "" && monitor.Spec.Database.Password != ""
}

// BuildCredentialsSecret returns the Secret that hands inline credentials to
// the collector pod.
//
// They used to be written into the pod template as plain values of PGUSER
// and PGPASSWORD. There the password could be read by everyone who may look
// at a Deployment, a ReplicaSet or a Pod, which is far more people than may
// read Secrets or the monitor, and it stayed in the ReplicaSets of earlier
// revisions after it was changed. The kubelet also rewrites such a value:
// "$$" becomes "$", and "$(NAME)" the value of another variable. A value
// taken from a Secret is neither shown in the pod nor rewritten.
func BuildCredentialsSecret(monitor *pgpilotv1.PgpilotMonitor) *corev1.Secret {
	return &corev1.Secret{
		APIVersion: "v1", Kind: "Secret",
		Name:      CredentialsSecretName(monitor.Name),
		Namespace: monitor.Namespace,
		Labels:    SystemLabels(monitor.Name),
		Type:      corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			generatedUsernameKey: []byte(monitor.Spec.Database.Username),
			generatedPasswordKey: []byte(monitor.Spec.Database.Password),
		},
	}
}

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
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
)

// awkward is a password the kubelet would rewrite if it were a plain value
// of an environment variable: "$$" to "$", "$(PGUSER)" to the user name.
// The blanks at its ends are part of it.
const awkward = " Pa$$w0rd-$(PGUSER) \"q\" \u00e9\t"

func inlineMonitor(username, password string) *pgpilotv1.PgpilotMonitor {
	monitor := newTestMonitor()
	monitor.Spec.Database.CredentialsSecret = nil
	monitor.Spec.Database.Username, monitor.Spec.Database.Password = username, password
	return monitor
}

func TestBuildCredentialsSecret(t *testing.T) {
	monitor := inlineMonitor("alice", awkward)
	secret := BuildCredentialsSecret(monitor)

	if secret.APIVersion != "v1" || secret.Kind != "Secret" {
		t.Errorf("type %s %s; server-side apply needs both", secret.APIVersion, secret.Kind)
	}
	if secret.Name != "pgpilot-test-db-credentials" || secret.Name != CredentialsSecretName(monitor.Name) ||
		secret.Namespace != testMonitorNS {
		t.Errorf("name %s/%s", secret.Namespace, secret.Name)
	}
	if secret.Type != corev1.SecretTypeOpaque {
		t.Errorf("type %q", secret.Type)
	}
	// The value as it is, byte for byte.
	if len(secret.Data) != 2 || string(secret.Data["username"]) != "alice" || string(secret.Data["password"]) != awkward {
		t.Errorf("data %q", secret.Data)
	}
	if len(secret.StringData) != 0 {
		t.Errorf("stringData %v: it is write-only, and would be applied again at every reconcile", secret.StringData)
	}
	for key, want := range SystemLabels(monitor.Name) {
		if secret.Labels[key] != want {
			t.Errorf("label %s is %q, want %q", key, secret.Labels[key], want)
		}
	}
}

func TestHasInlineCredentials(t *testing.T) {
	for name, tc := range map[string]struct {
		username, password string
		want               bool
	}{
		"both":               {"alice", "s3cret", true},
		"a username alone":   {"alice", "", false},
		"a password alone":   {"", "s3cret", false},
		"neither":            {"", "", false},
		"blanks are a value": {" ", " ", true},
	} {
		if got := HasInlineCredentials(inlineMonitor(tc.username, tc.password)); got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}

// The pod template of a Deployment is read by many who may read neither
// Secrets nor the monitor. Whatever the credentials are given as, the pod
// gets a reference, never a value.
func TestBuildDeployment_EnvVars_NeverHoldAValue(t *testing.T) {
	both := inlineMonitor("alice", awkward)
	both.Spec.Database.CredentialsSecret = &pgpilotv1.CredentialsSecretRef{Name: "other", UsernameKey: "user", PasswordKey: "pass"}
	// A username alone is no inline credential: the pod must not be pointed
	// at a generated Secret the controller never writes.
	usernameOnly := newTestMonitor()
	usernameOnly.Spec.Database.Username = "alice"
	custom := newTestMonitor()
	custom.Spec.Database.CredentialsSecret = &pgpilotv1.CredentialsSecretRef{Name: "pg-creds", UsernameKey: "user", PasswordKey: "pass"}

	cases := map[string]struct {
		monitor                  *pgpilotv1.PgpilotMonitor
		secret, userKey, passKey string
	}{
		"inline":                      {inlineMonitor("alice", awkward), "pgpilot-test-db-credentials", "username", "password"},
		"a Secret of the monitor":     {newTestMonitor(), "pg-creds", "username", "password"},
		"a Secret with its own keys":  {custom, "pg-creds", "user", "pass"},
		"both stored: inline is used": {both, "pgpilot-test-db-credentials", "username", "password"},
		"a username alone next to it": {usernameOnly, "pg-creds", "username", "password"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dep := BuildDeployment(tc.monitor, MergedMetrics{}, PodTemplateVersions{Config: "h"})
			env := dep.Spec.Template.Spec.Containers[0].Env
			if len(env) != 2 || env[0].Name != "PGUSER" || env[1].Name != "PGPASSWORD" {
				t.Fatalf("env %+v", env)
			}
			for i, key := range []string{tc.userKey, tc.passKey} {
				ref := env[i].ValueFrom
				if env[i].Value != "" || ref == nil || ref.SecretKeyRef == nil ||
					ref.SecretKeyRef.Name != tc.secret || ref.SecretKeyRef.Key != key || ref.SecretKeyRef.Optional != nil {
					t.Errorf("%s: value %q, from %+v; want key %q of Secret %q", env[i].Name, env[i].Value, ref, key, tc.secret)
				}
			}
			raw, err := json.Marshal(dep)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"alice", "Pa$$w0rd", "w0rd"} {
				if HasInlineCredentials(tc.monitor) && strings.Contains(string(raw), secret) {
					t.Errorf("the Deployment holds %q", secret)
				}
			}
		})
	}

	// The generated Secret has its own two keys, whatever keys a reference
	// that is stored next to the inline credentials names.
	t.Run("both stored: the generated Secret has its own keys", func(t *testing.T) {
		data := BuildCredentialsSecret(both).Data
		if len(data) != 2 || string(data["username"]) != "alice" || string(data["password"]) != awkward {
			t.Errorf("data %q", data)
		}
	})

	t.Run("no credentials at all", func(t *testing.T) {
		monitor := inlineMonitor("", "")
		dep := BuildDeployment(monitor, MergedMetrics{}, PodTemplateVersions{Config: "h"})
		if env := dep.Spec.Template.Spec.Containers[0].Env; len(env) != 0 {
			t.Errorf("env %+v", env)
		}
	})
}

// ReplicaSets of earlier revisions keep what earlier pod templates held,
// which up to v1.0.2 was the inline password.
func TestBuildDeployment_KeepsNoEarlierRevisions(t *testing.T) {
	dep := BuildDeployment(newTestMonitor(), MergedMetrics{}, PodTemplateVersions{Config: "h"})
	if limit := dep.Spec.RevisionHistoryLimit; limit == nil || *limit != 0 {
		t.Errorf("revisionHistoryLimit %v, want 0", limit)
	}
}

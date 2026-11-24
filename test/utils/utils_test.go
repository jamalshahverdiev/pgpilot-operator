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

package utils

import (
	"strconv"
	"strings"
	"testing"
)

func TestKindClusterName(t *testing.T) {
	tests := []struct {
		name  string
		set   bool
		value string
		want  string
	}{
		{name: "unset falls back to kind's default", want: "kind"},
		{name: "empty falls back to kind's default", set: true, value: "", want: "kind"},
		{name: "set", set: true, value: "pgpilot-operator-test-e2e", want: "pgpilot-operator-test-e2e"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.set {
				t.Setenv("KIND_CLUSTER", tt.value)
			} else {
				// t.Setenv registers the restore of the original value;
				// the variable itself has to be absent for this case.
				t.Setenv("KIND_CLUSTER", "")
				unsetenv(t, "KIND_CLUSTER")
			}
			if got := KindClusterName(); got != tt.want {
				t.Errorf("KindClusterName() = %q, want %q", got, tt.want)
			}
		})
	}
}

// A bare `go test -tags=e2e` must not adopt a developer's default cluster.
func TestRequireKindCluster(t *testing.T) {
	t.Run("unset", func(t *testing.T) {
		t.Setenv("KIND_CLUSTER", "")
		unsetenv(t, "KIND_CLUSTER")
		name, err := RequireKindCluster()
		if err == nil {
			t.Fatalf("RequireKindCluster() = %q, want an error", name)
		}
		if !strings.Contains(err.Error(), "make test-e2e") {
			t.Errorf("error must say how to run the suite, got: %v", err)
		}
	})
	for _, blank := range []string{"", "   ", "\n"} {
		t.Run("blank "+strconv.Quote(blank), func(t *testing.T) {
			t.Setenv("KIND_CLUSTER", blank)
			if name, err := RequireKindCluster(); err == nil {
				t.Fatalf("RequireKindCluster() = %q, want an error", name)
			}
		})
	}
	t.Run("set", func(t *testing.T) {
		t.Setenv("KIND_CLUSTER", " pgpilot-operator-test-e2e\n")
		name, err := RequireKindCluster()
		if err != nil || name != "pgpilot-operator-test-e2e" {
			t.Fatalf("RequireKindCluster() = %q, %v", name, err)
		}
	})
}

func TestKindContextName(t *testing.T) {
	if got := KindContextName("pgpilot-operator-test-e2e"); got != "kind-pgpilot-operator-test-e2e" {
		t.Errorf("KindContextName = %q", got)
	}
}

// The guard is what keeps the suite's install/undeploy/uninstall steps away
// from a cluster that was not created for it.
func TestVerifyKubeContext(t *testing.T) {
	const cluster = "pgpilot-operator-test-e2e"

	accepted := []string{
		"kind-pgpilot-operator-test-e2e",
		"kind-pgpilot-operator-test-e2e\n",
		"  kind-pgpilot-operator-test-e2e  ",
	}
	for _, current := range accepted {
		if err := VerifyKubeContext(current, cluster); err != nil {
			t.Errorf("VerifyKubeContext(%q) = %v, want nil", current, err)
		}
	}

	rejected := map[string]string{
		"production cluster":             "prod-eu-west-1",
		"another kind cluster":           "kind-kind",
		"kind cluster with longer name":  "kind-pgpilot-operator-test-e2e-2",
		"kind cluster with shorter name": "kind-pgpilot-operator-test",
		"cluster name without prefix":    "pgpilot-operator-test-e2e",
		"different case":                 "KIND-pgpilot-operator-test-e2e",
		"no current context":             "",
		"whitespace only":                " \n",
		"minikube":                       "minikube",
		"docker desktop":                 "docker-desktop",
	}
	for name, current := range rejected {
		t.Run(name, func(t *testing.T) {
			err := VerifyKubeContext(current, cluster)
			if err == nil {
				t.Fatalf("VerifyKubeContext(%q) = nil, want an error", current)
			}
			if !strings.Contains(err.Error(), "kind-"+cluster) {
				t.Errorf("error must name the expected context, got: %v", err)
			}
			if !strings.Contains(err.Error(), "make test-e2e") {
				t.Errorf("error must say how to run the suite, got: %v", err)
			}
		})
	}
}

func TestGetNonEmptyLines(t *testing.T) {
	got := GetNonEmptyLines("a\n\nb\n\n\nc\n")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("GetNonEmptyLines = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
	if lines := GetNonEmptyLines("\n\n"); len(lines) != 0 {
		t.Errorf("GetNonEmptyLines of blank input = %q, want none", lines)
	}
}

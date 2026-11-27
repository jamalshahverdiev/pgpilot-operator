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

package chart

import (
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// permissions flattens RBAC rules into "group/resource:verb" entries, so two
// roles that group their rules differently can be compared.
func permissions(t *testing.T, role object) []string {
	t.Helper()
	rules, _ := role["rules"].([]any)
	if len(rules) == 0 {
		t.Fatalf("role %v has no rules", nested(role, "metadata", "name"))
	}
	set := map[string]bool{}
	for _, r := range rules {
		rule, _ := r.(object)
		groups, _ := rule["apiGroups"].([]any)
		resources, _ := rule["resources"].([]any)
		verbs, _ := rule["verbs"].([]any)
		if len(groups) == 0 || len(resources) == 0 || len(verbs) == 0 {
			t.Fatalf("rule without apiGroups, resources or verbs: %v", rule)
		}
		for _, g := range groups {
			for _, res := range resources {
				for _, v := range verbs {
					set[g.(string)+"/"+res.(string)+":"+v.(string)] = true
				}
			}
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func managerRoleOfChart(t *testing.T) object {
	t.Helper()
	var found []object
	for _, o := range ofKind(render(t), "ClusterRole") {
		if strings.HasSuffix(nestedString(o, "metadata", "name"), "-manager") {
			found = append(found, o)
		}
	}
	if len(found) != 1 {
		t.Fatalf("chart renders %d manager ClusterRoles, want 1", len(found))
	}
	return found[0]
}

// config/rbac/role.yaml is generated from the +kubebuilder:rbac markers next
// to the code that needs each permission. The chart's ClusterRole is written
// by hand; the two install paths must grant the operator exactly the same.
func TestChartRBAC_MatchesGeneratedRole(t *testing.T) {
	generated := parseYAML(t, readFile(t, filepath.Join(repoRoot, "config", "rbac", "role.yaml")), "role.yaml")

	want := permissions(t, generated)
	got := permissions(t, managerRoleOfChart(t))

	missing, extra := difference(want, got), difference(got, want)
	if len(missing) != 0 {
		t.Errorf("the chart's ClusterRole lacks permissions the operator needs:\n  %s", strings.Join(missing, "\n  "))
	}
	if len(extra) != 0 {
		t.Errorf("the chart's ClusterRole grants more than config/rbac/role.yaml:\n  %s", strings.Join(extra, "\n  "))
	}
}

// The operator reads the one Secret a monitor names, and writes the one it
// generates for a monitor with inline credentials: created and updated by
// server-side apply (a patch), deleted when the monitor has none any more.
// Listing or watching Secrets would let it, and whoever compromises it, read
// every Secret in the cluster; neither is ever needed, nor is a plain update
// or deleting them in bulk.
func TestRBAC_SecretsAreReadOneAtATime(t *testing.T) {
	roles := map[string]object{
		"config/rbac/role.yaml": parseYAML(t,
			readFile(t, filepath.Join(repoRoot, "config", "rbac", "role.yaml")), "role.yaml"),
		"chart ClusterRole": managerRoleOfChart(t),
	}
	for name, role := range roles {
		t.Run(name, func(t *testing.T) {
			var onSecrets []string
			for _, p := range permissions(t, role) {
				if strings.HasPrefix(p, "/secrets:") || strings.Contains(p, "/*:") || strings.HasPrefix(p, "*/") {
					onSecrets = append(onSecrets, p)
				}
			}
			want := []string{"/secrets:create", "/secrets:delete", "/secrets:get", "/secrets:patch"}
			slices.Sort(onSecrets)
			if !slices.Equal(onSecrets, want) {
				t.Errorf("permissions on Secrets = %v, want exactly %v", onSecrets, want)
			}
		})
	}
}

func difference(a, b []string) []string {
	in := make(map[string]bool, len(b))
	for _, x := range b {
		in[x] = true
	}
	var out []string
	for _, x := range a {
		if !in[x] {
			out = append(out, x)
		}
	}
	return out
}

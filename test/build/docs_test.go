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

package build

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// upgradeCheckInput is what `kubectl get pgpilotmonitors,pgpilotmetriclibraries
// -A -o json` returns: objects that break a rule introduced with the stricter
// validation, next to objects that do not.
const upgradeCheckInput = `{"items": [
  {"kind": "PgpilotMonitor", "metadata": {"namespace": "a", "name": "fine-preset"},
   "spec": {"metrics": {"preset": "basic"}, "sinks": {"prometheus": {"enabled": true}}}},
  {"kind": "PgpilotMonitor", "metadata": {"namespace": "a", "name": "fine-custom"},
   "spec": {"metrics": {"custom": [
              {"name": "m", "sqls": {"0": "SELECT 1", "14": "SELECT 1", "999": "SELECT 1"}}]},
            "sinks": {}}},
  {"kind": "PgpilotMonitor", "metadata": {"namespace": "a", "name": "fine-grpc-only"},
   "spec": {"metrics": {"preset": "basic"},
            "sinks": {"prometheus": {"enabled": false}, "grpc": {"enabled": true, "endpoint": "r:9000"}}}},
  {"kind": "PgpilotMonitor", "metadata": {"namespace": "a", "name": "fine-one-role"},
   "spec": {"metrics": {"custom": [
              {"name": "m", "sqls": {"14": "SELECT 1"}, "masterOnly": true, "standbyOnly": false}]},
            "sinks": {"prometheus": {"enabled": true}}}},
  {"kind": "PgpilotMetricLibrary", "metadata": {"namespace": "a", "name": "fine-library"},
   "spec": {"metrics": [{"name": "m", "sqls": {"13": "SELECT 1"}}]}},
  {"kind": "PgpilotMonitor", "metadata": {"namespace": "a", "name": "fine-uses-library"},
   "spec": {"metrics": {"fromLibraries": [{"name": "fine-library"}, {"name": "library-wildcard"}]},
            "sinks": {"prometheus": {"enabled": true}}}},
  {"kind": "PgpilotMonitor", "metadata": {"namespace": "a", "name": "fine-overrides-library"},
   "spec": {"metrics": {"fromLibraries": [{"name": "fine-library"}],
                        "custom": [{"name": "m", "sqls": {"13": " "}}, {"name": "_m2", "sqls": {"0": "SELECT 1"}}]},
            "sinks": {"prometheus": {"enabled": true}}}},

  {"kind": "PgpilotMonitor", "metadata": {"namespace": "b", "name": "wildcard-key"},
   "spec": {"metrics": {"custom": [{"name": "m", "sqls": {"14": "SELECT 1", "*": "SELECT 1"}}]},
            "sinks": {"prometheus": {"enabled": true}}}},
  {"kind": "PgpilotMonitor", "metadata": {"namespace": "b", "name": "version-num-key"},
   "spec": {"metrics": {"preset": "basic", "custom": [{"name": "m", "sqls": {"170000": "SELECT 1"}}]},
            "sinks": {"prometheus": {"enabled": true}}}},
  {"kind": "PgpilotMonitor", "metadata": {"namespace": "b", "name": "leading-zero-key"},
   "spec": {"metrics": {"custom": [
              {"name": "ok", "sqls": {"14": "SELECT 1"}}, {"name": "m", "sqls": {"014": "SELECT 1"}}]},
            "sinks": {"prometheus": {"enabled": true}}}},
  {"kind": "PgpilotMonitor", "metadata": {"namespace": "b", "name": "both-roles"},
   "spec": {"metrics": {"custom": [{"name": "m", "sqls": {"14": "SELECT 1"}, "masterOnly": true, "standbyOnly": true}]},
            "sinks": {"prometheus": {"enabled": true}}}},
  {"kind": "PgpilotMonitor", "metadata": {"namespace": "b", "name": "no-sink"},
   "spec": {"metrics": {"preset": "basic"}, "sinks": {"prometheus": {"enabled": false}}}},
  {"kind": "PgpilotMonitor", "metadata": {"namespace": "b", "name": "no-sink-grpc-off"},
   "spec": {"metrics": {"preset": "basic"},
            "sinks": {"prometheus": {"enabled": false}, "grpc": {"enabled": false, "endpoint": ""}}}},
  {"kind": "PgpilotMetricLibrary", "metadata": {"namespace": "b", "name": "library-wildcard"},
   "spec": {"metrics": [{"name": "ok", "sqls": {"14": "SELECT 1"}}, {"name": "m", "sqls": {"*": "SELECT 1"}}]}},
  {"kind": "PgpilotMetricLibrary", "metadata": {"namespace": "b", "name": "library-both-roles"},
   "spec": {"metrics": [{"name": "m", "sqls": {"14": "SELECT 1"}, "masterOnly": true, "standbyOnly": true}]}},
  {"kind": "PgpilotMetricLibrary", "metadata": {"namespace": "b", "name": "library-duplicate-name"},
   "spec": {"metrics": [{"name": "m", "sqls": {"14": "SELECT 1"}}, {"name": "m", "sqls": {"14": "SELECT 2"}}]}},
  {"kind": "PgpilotMetricLibrary", "metadata": {"namespace": "b", "name": "library-empty-statement"},
   "spec": {"metrics": [{"name": "ok", "sqls": {"14": "SELECT 1", "15": ""}}]}},
  {"kind": "PgpilotMetricLibrary", "metadata": {"namespace": "b", "name": "library-no-statement"},
   "spec": {"metrics": [{"name": "ok", "sqls": {"14": "SELECT 1"}}, {"name": "none"}]}},
  {"kind": "PgpilotMetricLibrary", "metadata": {"namespace": "b", "name": "library-upper-case-name"},
   "spec": {"metrics": [{"name": "ordersQueue", "sqls": {"14": "SELECT 1"}}]}},
  {"kind": "PgpilotMetricLibrary", "metadata": {"namespace": "b", "name": "library-name-of-two-lines"},
   "spec": {"metrics": [{"name": "orders\nqueue", "sqls": {"14": "SELECT 1"}}]}},
  {"kind": "PgpilotMetricLibrary", "metadata": {"namespace": "b", "name": "library-long-name"},
   "spec": {"metrics": [{"name": "m234567890123456789012345678901234567890123456789012345678901234",
                         "sqls": {"14": "SELECT 1"}}]}},
  {"kind": "PgpilotMetricLibrary", "metadata": {"namespace": "b", "name": "library-key-with-newline"},
   "spec": {"metrics": [{"name": "m", "sqls": {"14\n": "SELECT 1"}}]}},
  {"kind": "PgpilotMonitor", "metadata": {"namespace": "b", "name": "duplicate-name"},
   "spec": {"metrics": {"preset": "basic", "custom": [
              {"name": "m", "sqls": {"14": "SELECT 1"}}, {"name": "m", "sqls": {"14": "SELECT 2"}}]},
            "sinks": {"prometheus": {"enabled": true}}}},
  {"kind": "PgpilotMonitor", "metadata": {"namespace": "b", "name": "empty-statement"},
   "spec": {"metrics": {"custom": [{"name": "m", "sqls": {"14": ""}}]}, "sinks": {"prometheus": {"enabled": true}}}},
  {"kind": "PgpilotMonitor", "metadata": {"namespace": "b", "name": "uses-library-wildcard"},
   "spec": {"metrics": {"preset": "basic", "fromLibraries": [{"name": "fine-library"}, {"name": "library-wildcard"}]},
            "sinks": {"prometheus": {"enabled": true}}}}
]}`

// The upgrade notes give a command to find objects the stricter validation
// affects. A command that prints nothing because it is wrong reads exactly
// like "nothing to fix", so it is run here against known input.
func TestChangelog_UpgradeCheckCommandFindsAffectedObjects(t *testing.T) {
	jq, err := exec.LookPath("jq")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("jq is required in CI but was not found: %v", err)
		}
		t.Skip("jq not found in PATH")
	}

	changelog := readRepoFile(t, "CHANGELOG.md")
	m := regexp.MustCompile(`(?s)kubectl get pgpilotmonitors,pgpilotmetriclibraries -A -o json \| jq -r '(.*?)'\n`).
		FindStringSubmatch(changelog)
	if m == nil {
		t.Fatal("CHANGELOG.md no longer contains the upgrade check command")
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.Command(jq, "-r", m[1])
	cmd.Stdin = strings.NewReader(upgradeCheckInput)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("jq failed: %v\n%s", err, stderr.String())
	}

	got := strings.Fields(strings.ReplaceAll(strings.TrimSpace(stdout.String()), " ", ":"))
	sort.Strings(got)
	// Not reported, among others: a monitor in namespace a that names a
	// library "library-wildcard" (the broken one of that name is in b), and
	// one that overrides a metric of its library by name.
	want := []string{
		"PgpilotMetricLibrary:b/library-both-roles",
		"PgpilotMetricLibrary:b/library-duplicate-name",
		"PgpilotMetricLibrary:b/library-empty-statement",
		"PgpilotMetricLibrary:b/library-key-with-newline",
		"PgpilotMetricLibrary:b/library-long-name",
		"PgpilotMetricLibrary:b/library-name-of-two-lines",
		"PgpilotMetricLibrary:b/library-no-statement",
		"PgpilotMetricLibrary:b/library-upper-case-name",
		"PgpilotMetricLibrary:b/library-wildcard",
		"PgpilotMonitor:b/both-roles",
		"PgpilotMonitor:b/duplicate-name",
		"PgpilotMonitor:b/empty-statement",
		"PgpilotMonitor:b/leading-zero-key",
		"PgpilotMonitor:b/no-sink",
		"PgpilotMonitor:b/no-sink-grpc-off",
		"PgpilotMonitor:b/uses-library-wildcard",
		"PgpilotMonitor:b/version-num-key",
		"PgpilotMonitor:b/wildcard-key",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the command reported:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// The example dashboard is what most users start from. A monitor is known by
// its namespace and its name together; a query that selects or groups by the
// name alone draws two monitors of one name as one.
func TestDashboard_TellsMonitorsApartByNamespace(t *testing.T) {
	var dashboard struct {
		Panels []struct {
			Title   string `json:"title"`
			Type    string `json:"type"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
			Transformations []struct {
				Options struct {
					ExcludeByName map[string]bool `json:"excludeByName"`
				} `json:"options"`
			} `json:"transformations"`
		} `json:"panels"`
		Templating struct {
			List []struct {
				Name string `json:"name"`
			} `json:"list"`
		} `json:"templating"`
	}
	if err := json.Unmarshal([]byte(readRepoFile(t, "examples/grafana/pgpilot-overview.json")), &dashboard); err != nil {
		t.Fatal(err)
	}

	variables := make([]string, 0, len(dashboard.Templating.List))
	for _, v := range dashboard.Templating.List {
		variables = append(variables, v.Name)
	}
	at := func(name string) int { return slices.Index(variables, name) }
	if at("namespace") < 0 || at("dbname") < 0 || at("namespace") > at("dbname") {
		t.Errorf("variables %v: want namespace, and before dbname, which is chosen within it", variables)
	}

	grouping := regexp.MustCompile(`by \(([^)]*)\)`)
	exprs := 0
	for _, panel := range dashboard.Panels {
		for _, target := range panel.Targets {
			if target.Expr == "" {
				continue
			}
			exprs++
			if !strings.Contains(target.Expr, `namespace=~"$namespace"`) ||
				!strings.Contains(target.Expr, `dbname=~"$dbname"`) {
				t.Errorf("panel %q: %s does not select by namespace and dbname", panel.Title, target.Expr)
			}
			for _, by := range grouping.FindAllStringSubmatch(target.Expr, -1) {
				if strings.Contains(by[1], "dbname") && !strings.Contains(by[1], "namespace") {
					t.Errorf("panel %q groups by (%s): two monitors of one name become one", panel.Title, by[1])
				}
			}
		}
		// A table shows every label as a column. The namespace is one to
		// show; a scraper that sets its own leaves pgwatch's behind as
		// exported_namespace, which is one to hide.
		for _, transformation := range panel.Transformations {
			hidden := transformation.Options.ExcludeByName
			if hidden != nil && (!hidden["exported_namespace"] || hidden["namespace"]) {
				t.Errorf("panel %q hides %v", panel.Title, hidden)
			}
		}
	}
	if exprs < 10 {
		t.Errorf("only %d queries were looked at: has the dashboard changed its shape?", exprs)
	}
}

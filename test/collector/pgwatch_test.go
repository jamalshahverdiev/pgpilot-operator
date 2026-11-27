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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	yaml "gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
	"github.com/jamalshahverdiev/pgpilot-operator/internal/builder"
)

// defaultImage is the image of a monitor that names none.
func defaultImage(t *testing.T) string {
	t.Helper()
	monitor := newMonitor("default-image", pgpilotv1.MetricsSpec{Preset: "basic"})
	dep := builder.BuildDeployment(monitor, builder.MergedMetrics{Preset: "basic"}, builder.PodTemplateVersions{})
	return dep.Spec.Template.Spec.Containers[0].Image
}

// A monitor with metrics of its own is handed the operator's copy of
// pgwatch's metric registry, in place of the one inside the image, and the
// names of the presets are checked against that copy for every monitor. The
// two have to be the same file, or a monitor would run one release's binary
// with another release's SQL.
func TestEmbeddedRegistryIsTheOneInsideTheDefaultImage(t *testing.T) {
	image := defaultImage(t)
	if want := builder.DefaultImageRepository + ":" + builder.PgwatchVersion; image != want {
		t.Fatalf("default image is %s, want %s", image, want)
	}

	name := "pgpilot-collector-test-" + runID + "-registry"
	if out, err := docker("create", "--name", name, "--label", runLabel, image); err != nil {
		t.Fatalf("create a container of %s: %v\n%s", image, err, out)
	}
	t.Cleanup(func() { _, _ = docker("rm", "--force", name) })

	fromImage := filepath.Join(t.TempDir(), "metrics.yaml")
	if out, err := docker("cp", name+":/pgwatch/metrics/metrics.yaml", fromImage); err != nil {
		t.Fatalf("copy the registry out of %s: %v\n%s", image, err, out)
	}
	inImage, err := os.ReadFile(fromImage)
	if err != nil {
		t.Fatal(err)
	}
	embedded, err := os.ReadFile(filepath.Join("..", "..", "internal", "builder", "pgwatchdata", "metrics.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(inImage, embedded) {
		t.Errorf("internal/builder/pgwatchdata/metrics.yaml (%d bytes) is not the registry inside %s (%d bytes); "+
			"run make update-pgwatch-registry PGWATCH_VERSION=%s", len(embedded), image, len(inImage), builder.PgwatchVersion)
	}
}

// The binary in the default image must be the release the constant names.
func TestDefaultImageIsTheReleaseTheOperatorIsBuiltFor(t *testing.T) {
	// pgwatch has no --version; its help text ends with the build details.
	out, _ := docker("run", "--rm", defaultImage(t), "--help")
	version := regexp.MustCompile(`(?m)^\s*Version:\s+v?(\S+)\s*$`).FindStringSubmatch(out)
	if version == nil {
		t.Fatalf("no version in the output of pgwatch --help:\n%s", out)
	}
	if version[1] != builder.PgwatchVersion {
		t.Errorf("the default image runs pgwatch %s, the operator is built for %s", version[1], builder.PgwatchVersion)
	}
}

// What every example in the docs uses: a preset and nothing else.
func TestPresetOnly(t *testing.T) {
	t.Parallel()
	monitor := newMonitor("preset-only", pgpilotv1.MetricsSpec{Preset: "basic"})
	c := startCollector(t, monitor, nil, defaultSecrets())

	got := c.eventually("first samples of preset basic", all(
		up(monitor.Name),
		// One series of each metric of the preset, as the quickstart and
		// the dashboard name them.
		present(monitor, "pgwatch_db_stats_numbackends", "pgwatch_db_size_size_b", "pgwatch_wal_xlog_location_b"),
	))

	// The web UI and REST API, which would hand the database credentials to
	// anyone who can reach the pod, must stay off.
	if c.accepts(8080) {
		t.Errorf("something listens on port 8080: the web server is not disabled")
	}
	if !c.accepts(c.port) {
		t.Errorf("the metrics port does not accept connections: the pod's probes would fail")
	}
	// Nothing but what the preset asks for: a preset resolved from another
	// registry than the image's shows up as extra or missing metrics.
	for _, name := range got.names("pgwatch_") {
		known := false
		for _, metric := range []string{"instance_up", "db_stats", "db_size", "wal", "exporter"} {
			known = known || strings.HasPrefix(name, "pgwatch_"+metric)
		}
		if !known {
			t.Errorf("series %s does not belong to preset basic", name)
		}
	}
	c.expectCleanLog()
}

// A preset combined with custom metrics: pgwatch cannot add the two itself,
// so the operator spells the preset out and ships the whole registry.
func TestPresetWithLibraryAndCustomMetrics(t *testing.T) {
	t.Parallel()
	library := pgpilotv1.PgpilotMetricLibrary{
		Name: "shared"}
	library.Spec.Metrics = []pgpilotv1.MetricDefinition{{
		Name:     "library_metric",
		Interval: &metav1.Duration{Duration: 5 * time.Second},
		SQLs: map[string]string{
			"14": "SELECT (extract(epoch from now()) * 1e9)::int8 AS epoch_ns, 7::int8 AS answer",
		},
		Gauges: []string{"*"},
	}}

	monitor := newMonitor("preset-custom", pgpilotv1.MetricsSpec{
		Preset:        "basic",
		FromLibraries: []pgpilotv1.LibraryRef{{Name: "shared"}},
		Custom: []pgpilotv1.MetricDefinition{{
			Name:     "queue_depth",
			Interval: &metav1.Duration{Duration: 5 * time.Second},
			SQLs: map[string]string{
				// The lower key must lose against the higher one on this
				// server, and tag_ columns must become labels.
				"11": "SELECT (extract(epoch from now()) * 1e9)::int8 AS epoch_ns, 'old' AS tag_queue, 1::int8 AS depth",
				"14": "SELECT (extract(epoch from now()) * 1e9)::int8 AS epoch_ns, 'jobs' AS tag_queue, 42::int8 AS depth",
			},
			Gauges: []string{"*"},
		}},
	})
	c := startCollector(t, monitor, []pgpilotv1.PgpilotMetricLibrary{library}, defaultSecrets())

	got := c.eventually("preset, library and custom metrics together", all(
		up(monitor.Name),
		present(monitor, "pgwatch_db_stats_numbackends", "pgwatch_db_size_size_b", "pgwatch_wal_xlog_location_b",
			"pgwatch_queue_depth_depth", "pgwatch_library_metric_answer"),
	))

	depth := got.find("pgwatch_queue_depth_depth", map[string]string{"dbname": monitor.Name})
	if len(depth) != 1 || depth[0].value != 42 || depth[0].labels["queue"] != "jobs" {
		t.Errorf(`want pgwatch_queue_depth_depth{queue="jobs"} 42, from the SQL of the highest matching version; got %+v`,
			depth)
	}
	if answer := got.find("pgwatch_library_metric_answer", nil); len(answer) != 1 || answer[0].value != 7 {
		t.Errorf("want pgwatch_library_metric_answer 7, got %+v", answer)
	}
	c.expectCleanLog()
}

// Custom metrics without a preset: the generated metrics.yaml then holds
// nothing but the monitor's own definitions.
func TestCustomMetricsOnly(t *testing.T) {
	t.Parallel()
	monitor := newMonitor("custom-only", pgpilotv1.MetricsSpec{
		Custom: []pgpilotv1.MetricDefinition{{
			Name:     "queue_depth",
			Interval: &metav1.Duration{Duration: 5 * time.Second},
			SQLs:     map[string]string{"14": "SELECT (extract(epoch from now()) * 1e9)::int8 AS epoch_ns, 42::int8 AS depth"},
			Gauges:   []string{"*"},
		}},
	})
	c := startCollector(t, monitor, nil, defaultSecrets())

	got := c.eventually("a custom metric on its own", present(monitor, "pgwatch_queue_depth_depth"))
	for _, name := range got.names("pgwatch_") {
		if !strings.HasPrefix(name, "pgwatch_queue_depth") && !strings.HasPrefix(name, "pgwatch_exporter") {
			t.Errorf("series %s was not asked for", name)
		}
	}
	c.expectCleanLog()
}

// knownFailure is a built-in metric whose query fails on a freshly installed
// PostgreSQL with a pg_monitor role.
type knownFailure struct {
	why string
	// from is the first PostgreSQL major version on which it fails; zero
	// means all of them.
	from int
}

// cannotWorkOnAStockServer lists those metrics. Anything else that fails is a
// metric the operator handed over broken.
var cannotWorkOnAStockServer = map[string]knownFailure{
	// Helper functions pgwatch can create with --create-helpers, which the
	// operator does not pass: they need PL/Python or a superuser.
	"cpu_load":              {why: "helper get_load_average()"},
	"psutil_cpu":            {why: "helper get_psutil_cpu()"},
	"psutil_mem":            {why: "helper get_psutil_mem()"},
	"psutil_disk":           {why: "helper get_psutil_disk()"},
	"psutil_disk_io_total":  {why: "helper get_psutil_disk_io_total()"},
	"vmstat":                {why: "helper get_vmstat()"},
	"smart_health_per_disk": {why: "helper get_smart_health_per_device()"},
	"backup_age_walg":       {why: "helper get_backup_age_walg()"},
	"backup_age_pgbackrest": {why: "helper get_backup_age_pgbackrest()"},
	// Extensions that are not installed.
	"stat_statements":               {why: "extension pg_stat_statements"},
	"stat_statements_calls":         {why: "extension pg_stat_statements"},
	"stat_statements_no_query_text": {why: "extension pg_stat_statements"},
	"reco_add_index":                {why: "extension pg_qualstats"},
	"buffercache_by_db":             {why: "extension pg_buffercache"},
	"buffercache_by_type":           {why: "extension pg_buffercache"},
	"table_bloat_approx_stattuple":  {why: "extension pgstattuple"},
	"table_bloat_approx_summary":    {why: "extension pgstattuple"},
	// pgwatch 6.2.0's own statements for PostgreSQL 19 do not run there
	// ("column sync_error_count does not exist", "missing FROM-clause entry
	// for table ut"). Both are in preset debug only. Drop them from this
	// list when a pgwatch release fixes them: the test then fails here.
	"subscription_stats": {why: "pgwatch 6.2.0 SQL does not run on PostgreSQL 19", from: 19},
	"table_stats_approx": {why: "pgwatch 6.2.0 SQL does not run on PostgreSQL 19", from: 19},
}

// registryPresets reads the presets from the registry the operator embeds.
func registryPresets(t *testing.T) map[string]map[string]int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "builder", "pgwatchdata", "metrics.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var registry struct {
		Presets map[string]struct {
			Metrics map[string]int `yaml:"metrics"`
		} `yaml:"presets"`
	}
	if err := yaml.Unmarshal(raw, &registry); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]int{}
	for name, preset := range registry.Presets {
		out[name] = preset.Metrics
	}
	return out
}

// Every preset a monitor may use has to start, connect, resolve all its
// metrics and get an answer for each from the server. Metrics that need an
// extension or a helper function fail on a stock PostgreSQL, and are listed
// above; any other failure means the preset is not what the operator takes
// it for.
//
// A monitor that only names a preset resolves it from the registry inside
// the image. One with a metric of its own is handed the operator's rendering
// of the registry, and the widest presets are run that way as well: that is
// where every built-in statement the operator passes on is put to a server.
func TestEveryPresetResolves(t *testing.T) {
	presets, err := builder.Presets()
	if err != nil {
		t.Fatal(err)
	}
	if len(presets) < 10 {
		t.Fatalf("only %d presets: %v", len(presets), presets)
	}
	members := registryPresets(t)
	type run struct {
		name, preset string
		spec         pgpilotv1.MetricsSpec
		rendered     bool
	}
	runs := make([]run, 0, len(presets)+2)
	for _, preset := range presets {
		runs = append(runs, run{name: preset, preset: preset, spec: pgpilotv1.MetricsSpec{Preset: preset}})
	}
	for _, preset := range []string{"exhaustive", "debug"} {
		if !slices.Contains(presets, preset) {
			t.Fatalf("preset %s left the registry: pick another wide one", preset)
		}
		runs = append(runs, run{
			name: preset + " from the operator's rendering", preset: preset, rendered: true,
			spec: pgpilotv1.MetricsSpec{Preset: preset, Custom: []pgpilotv1.MetricDefinition{
				{Name: "queue_depth", SQLs: map[string]string{"14": "SELECT 7 AS depth"}},
			}},
		})
	}
	for _, r := range runs {
		preset := r.preset
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			if got := builder.NeedsMetricsFile(builder.MergeMetrics(r.spec, nil)); got != r.rendered {
				t.Fatalf("the monitor is handed a metrics.yaml: %v, this run is about the other case", got)
			}
			suffix := ""
			if r.rendered {
				suffix = "-rendered"
			}
			monitor := newMonitor("preset-"+strings.ReplaceAll(preset, "_", "-")+suffix, r.spec)
			c := startCollector(t, monitor, nil, defaultSecrets())

			// Not every preset has instance_up (those for managed services
			// leave it out), and the recommendations only have rows when
			// there is something to recommend, which an empty database
			// gives no reason for. A successful login is what they all share.
			c.eventually("connection of preset "+preset, func(got samples) error {
				if !strings.Contains(c.logs(), "Connect OK") {
					return fmt.Errorf("not connected yet")
				}
				if preset == "recommendations" {
					return nil
				}
				for _, s := range got {
					if s.labels["dbname"] == monitor.Name && s.labels["env"] == "collector-test" {
						return nil
					}
				}
				return fmt.Errorf("no sample of the monitor yet: %v", got.names("pgwatch_"))
			})

			c.awaitFirstCollection()
			for _, metric := range c.failedMetrics() {
				if _, in := members[preset][metric]; !in {
					// The monitor's own metric is not of the preset, and
					// does not fail.
					t.Errorf("pgwatch tried to fetch %q, which is not in preset %s", metric, preset)
				}
				if known, expected := cannotWorkOnAStockServer[metric]; !expected || serverMajor < known.from {
					t.Errorf("metric %q of preset %s failed on PostgreSQL %d: %s",
						metric, preset, serverMajor, c.failureOf(metric))
				}
			}

			// The metrics pgwatch 6 added for PostgreSQL 19 have no SQL for
			// older servers and are never sent to one.
			if _, has := members[preset]["stat_lock"]; has && serverMajor >= 19 {
				c.eventually("stat_lock on PostgreSQL 19", func(got samples) error {
					for _, name := range got.names("pgwatch_stat_lock_") {
						if len(got.find(name, map[string]string{"dbname": monitor.Name})) > 0 {
							return nil
						}
					}
					return fmt.Errorf("no pgwatch_stat_lock_* series: %v", got.names("pgwatch_"))
				})
			}

			for line := range strings.SplitSeq(c.logs(), "\n") {
				lower := strings.ToLower(line)
				for _, bad := range []string{"metric definition not found", "read-only file system", "cannot unmarshal", "panic:"} {
					if strings.Contains(lower, bad) {
						t.Errorf("unexpected line in the collector's log: %s", line)
					}
				}
			}
			t.Logf("metrics that cannot work on a stock PostgreSQL %d and failed as expected: %v",
				serverMajor, c.failedMetrics())
		})
	}
}

// The numeric server settings moved from one series per setting to one
// series with the setting as a label when pgwatch replaced its settings
// metric. The upgrade notes tell users to rewrite their queries this way.
func TestSettingsAreExportedByName(t *testing.T) {
	t.Parallel()
	monitor := newMonitor("settings", pgpilotv1.MetricsSpec{Preset: "exhaustive_no_python"})
	c := startCollector(t, monitor, nil, defaultSecrets())

	got := c.eventually("settings of the server", func(got samples) error {
		want := map[string]string{"dbname": monitor.Name, "name": "max_connections"}
		if len(got.find("pgwatch_settings_num_value", want)) != 1 {
			return fmt.Errorf(`no pgwatch_settings_num_value{name="max_connections"}`)
		}
		return nil
	})
	if old := got.find("pgwatch_settings_max_connections", nil); len(old) != 0 {
		t.Errorf("pgwatch_settings_max_connections is still exported: the upgrade notes say it is gone")
	}
}

// A wrong password must not crash the collector: it keeps running and keeps
// its port open, which is why the pod stays Ready, and says so in the one
// place a user can see it.
func TestWrongPasswordKeepsTheCollectorRunning(t *testing.T) {
	t.Parallel()
	monitor := newMonitor("wrong-password", pgpilotv1.MetricsSpec{Preset: "basic"})
	creds := secrets{credentialsSecret: {"username": monitoringUser, "password": "not-the-password"}}
	c := startCollector(t, monitor, nil, creds)

	deadline := time.Now().Add(30 * time.Second)
	for !c.accepts(c.port) {
		if time.Now().After(deadline) {
			t.Fatalf("the metrics port never opened")
		}
		time.Sleep(time.Second)
	}
	time.Sleep(15 * time.Second)
	if !c.running() {
		t.Fatalf("the collector exited on a failed login; the docs say it keeps retrying")
	}
	if !c.accepts(c.port) {
		t.Errorf("the metrics port closed after the failed login")
	}
	got, err := c.scrape()
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	// The one signal a user has: the docs tell them to look at this series.
	found := got.find("pgwatch_instance_up", map[string]string{"dbname": monitor.Name})
	if len(found) != 1 || found[0].value != 0 {
		t.Errorf("want pgwatch_instance_up{dbname=%q} 0 while the login fails, got %+v", monitor.Name, found)
	}
	if found := got.names("pgwatch_db_"); len(found) != 0 {
		t.Errorf("database metrics without a login: %v", found)
	}
	if !strings.Contains(c.logs(), "password authentication failed") {
		t.Errorf("the log does not name the failed login, which is what troubleshooting.md tells users to look for")
	}
}

// A monitor that only names a preset runs on the registry inside its image,
// whichever pgwatch release that is. pgwatch 5.1.0 collects the server
// settings as one series per setting; from 6.0 on they are one series with
// the setting as a label (see TestSettingsAreExportedByName). Handed the
// operator's copy of the 6.2.0 registry, as every monitor was, a 5.1.0
// collector exported them the new way.
func TestAPinnedReleaseRunsOnItsOwnRegistry(t *testing.T) {
	t.Parallel()
	const pinned = "5.1.0"
	if pinned == builder.PgwatchVersion {
		t.Fatalf("the test needs a release other than the operator's own")
	}

	t.Run("with a preset alone", func(t *testing.T) {
		t.Parallel()
		monitor := newMonitor("pinned-preset", pgpilotv1.MetricsSpec{Preset: "exhaustive_no_python"})
		monitor.Spec.Image.Tag = pinned
		c := startCollector(t, monitor, nil, defaultSecrets())
		if !strings.HasSuffix(c.image, ":"+pinned) {
			t.Fatalf("the collector runs %s", c.image)
		}
		got := c.eventually("settings of the server, the way pgwatch "+pinned+" exports them", func(got samples) error {
			if len(got.find("pgwatch_settings_max_connections", map[string]string{"dbname": monitor.Name})) != 1 {
				return fmt.Errorf("no pgwatch_settings_max_connections")
			}
			return nil
		})
		if newer := got.find("pgwatch_settings_num_value", nil); len(newer) != 0 {
			t.Errorf("pgwatch_settings_num_value is exported: the collector was handed the registry of %s",
				builder.PgwatchVersion)
		}
	})

	// With metrics of its own a monitor is handed a metrics.yaml, which
	// takes the place of the registry in the image, and the built-in
	// definitions in it are those of the operator's release. The docs say
	// so, and that leaving the tag unset is the way to avoid it.
	t.Run("with metrics of its own it is handed the operator's", func(t *testing.T) {
		t.Parallel()
		monitor := newMonitor("pinned-custom", pgpilotv1.MetricsSpec{
			Preset: "exhaustive_no_python",
			Custom: []pgpilotv1.MetricDefinition{{Name: "queue_depth", SQLs: map[string]string{"14": "SELECT 7 AS depth"}}},
		})
		monitor.Spec.Image.Tag = pinned
		c := startCollector(t, monitor, nil, defaultSecrets())
		what := "settings of the server, the way pgwatch " + builder.PgwatchVersion + " exports them"
		c.eventually(what, func(got samples) error {
			want := map[string]string{"dbname": monitor.Name, "name": "max_connections"}
			if len(got.find("pgwatch_settings_num_value", want)) != 1 {
				return fmt.Errorf(`no pgwatch_settings_num_value{name="max_connections"}`)
			}
			return nil
		})
	})
}

// Inline credentials reach the collector through the Secret the operator
// generates for them: the pod names that Secret and its keys, and what it
// reads there logs in.
func TestInlineCredentials(t *testing.T) {
	t.Parallel()
	monitor := newMonitor("inline", pgpilotv1.MetricsSpec{Preset: "basic"})
	monitor.Spec.Database.CredentialsSecret = nil
	monitor.Spec.Database.Username, monitor.Spec.Database.Password = monitoringUser, monitoringPassword

	generated := builder.BuildCredentialsSecret(monitor)
	held := map[string]string{}
	for key, value := range generated.Data {
		held[key] = string(value)
	}
	// Only the generated Secret: a pod that still named the monitor's own
	// would find nothing here.
	c := startCollector(t, monitor, nil, secrets{generated.Name: held})
	c.eventually("first samples with inline credentials", up(monitor.Name))
	c.expectCleanLog()
}

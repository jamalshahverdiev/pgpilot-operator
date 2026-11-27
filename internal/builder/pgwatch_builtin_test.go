package builder

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
)

// postgresPresets are the presets of the embedded registry a monitor can
// use. The list is spelled out on purpose: when the registry is replaced, a
// preset that appeared or went away has to be looked at (and the docs
// updated) rather than slip through.
var postgresPresets = []string{
	"aiven", "aurora", "azure", "basic", "debug", "exhaustive", "exhaustive_no_python",
	"full", "gce", "minimal", "rds", "recommendations", "standard",
}

// foreignPresets exist in the registry for pgwatch's Prometheus-scraping
// source kinds. Their entries name metric families of other exporters.
var foreignPresets = []string{"patroni", "postgres-exporter-basic"}

// poolerPresets are for connection poolers: their statements are commands of
// the pooler's admin console, which a PostgreSQL server does not understand.
var poolerPresets = []string{"pgbouncer", "pgpool"}

func TestPgwatchVersion_MatchesTheRecordedRegistryVersion(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("pgwatchdata", "VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(raw)); got != PgwatchVersion {
		t.Errorf("pgwatchdata/VERSION says %q but PgwatchVersion is %q: the embedded registry and the "+
			"default image tag must be the same pgwatch release (make update-pgwatch-registry)", got, PgwatchVersion)
	}
}

// The registry must stay the file pgwatch released: SHA256SUMS is written
// when it is downloaded, so an edit by hand shows up here.
func TestBuiltinRegistry_IsUnmodified(t *testing.T) {
	sums := map[string]string{}
	f, err := os.Open(filepath.Join("pgwatchdata", "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			t.Fatalf("unexpected line in SHA256SUMS: %q", scanner.Text())
		}
		sums[fields[1]] = fields[0]
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"metrics.yaml", "LICENSE"} {
		want, ok := sums[name]
		if !ok {
			t.Errorf("SHA256SUMS has no entry for %s", name)
			continue
		}
		raw, err := os.ReadFile(filepath.Join("pgwatchdata", name))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("pgwatchdata/%s does not match SHA256SUMS (got %s, want %s): the file is taken from "+
				"pgwatch unchanged; refresh it with make update-pgwatch-registry", name, got, want)
		}
	}

	embedded := sha256.Sum256(builtinMetricsYAML)
	if got := hex.EncodeToString(embedded[:]); got != sums["metrics.yaml"] {
		t.Errorf("the embedded registry is not pgwatchdata/metrics.yaml")
	}
}

// pgwatch is BSD-3-Clause: its copyright notice has to travel with the copy.
func TestBuiltinRegistry_ComesWithItsLicence(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("pgwatchdata", "LICENSE"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"BSD 3-Clause License", "CYBERTEC PostgreSQL International GmbH"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("pgwatchdata/LICENSE does not contain %q", want)
		}
	}
}

func TestPresets_ListsWhatAMonitorCanUse(t *testing.T) {
	got, err := Presets()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, postgresPresets) {
		t.Errorf("usable presets changed with the registry:\n got  %v\n want %v", got, postgresPresets)
	}

	_, all := builtinForTest(t)
	if want := len(postgresPresets) + len(foreignPresets) + len(poolerPresets); len(all) != want {
		t.Errorf("the registry has %d presets, the test knows %d: classify the new ones", len(all), want)
	}
}

// Every metric of a usable preset needs a definition, or the collector logs
// "metric definition not found" for it and the preset is silently smaller
// than its name promises.
func TestBuiltinRegistry_PresetsAreFullyDefined(t *testing.T) {
	metrics, presets := builtinForTest(t)
	for _, name := range postgresPresets {
		preset, ok := presets[name]
		if !ok {
			t.Errorf("preset %q is missing from the registry", name)
			continue
		}
		if len(preset.Metrics) == 0 {
			t.Errorf("preset %q is empty", name)
		}
		for metric, interval := range preset.Metrics {
			if _, ok := metrics[metric]; !ok {
				t.Errorf("preset %q: metric %q has no definition", name, metric)
			}
			if interval <= 0 {
				t.Errorf("preset %q: metric %q has interval %d", name, metric, interval)
			}
		}
	}
}

func TestValidatePreset(t *testing.T) {
	if err := ValidatePreset(""); err != nil {
		t.Errorf("no preset must be valid, got %v", err)
	}
	for _, name := range postgresPresets {
		if err := ValidatePreset(name); err != nil {
			t.Errorf("preset %q must be valid, got %v", name, err)
		}
	}

	invalid := map[string]string{
		"exhaustiv": "is not a preset of pgwatch " + PgwatchVersion,
		"Basic":     "is not a preset of pgwatch " + PgwatchVersion,
		"settings":  "is not a preset of pgwatch " + PgwatchVersion, // a metric, not a preset
	}
	for _, name := range foreignPresets {
		invalid[name] = "Prometheus-type sources"
	}
	for _, name := range poolerPresets {
		invalid[name] = "is a pgwatch preset for " + name + " sources, not for a PostgreSQL database"
	}
	for name, wantMessage := range invalid {
		err := ValidatePreset(name)
		if !errors.Is(err, ErrInvalidPreset) {
			t.Errorf("preset %q: want ErrInvalidPreset, got %v", name, err)
			continue
		}
		if !strings.Contains(err.Error(), wantMessage) {
			t.Errorf("preset %q: message %q does not contain %q", name, err, wantMessage)
		}
		// The message is all a user sees in the status: it has to say what
		// they can write instead, and must not offer what is refused.
		if !strings.Contains(err.Error(), "available: aiven, aurora, azure, basic,") {
			t.Errorf("preset %q: message does not list the usable presets: %v", name, err)
		}
		for _, foreign := range slices.Concat(foreignPresets, poolerPresets) {
			if strings.Contains(strings.SplitN(err.Error(), "available:", 2)[1], foreign) {
				t.Errorf("preset %q: message offers %q, which is refused: %v", name, foreign, err)
			}
		}
	}
}

// builtinForTest returns the embedded registry as these tests look at it:
// its metrics, by name, and what its presets collect.
func builtinForTest(t *testing.T) (map[string]*yamlv3.Node, map[string]pgwatchPreset) {
	t.Helper()
	registry, err := loadBuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	return registry.metrics, registry.presets
}

// renderedMetrics is a metrics.yaml as these tests read it: through the
// operator's own structs. That the keys are the ones pgwatch reads is for
// TestRenderMetrics_OwnDefinitionIsWrittenWithTheKeysPgwatchReads to say.
type renderedMetrics struct {
	Metrics map[string]pgwatchMetric `yaml:"metrics"`
	Presets map[string]pgwatchPreset `yaml:"presets"`
}

// renderedConfig is what the collector reads back from the ConfigMap.
type renderedConfig struct {
	source pgwatchSource
	// hasMetricsFile says whether the ConfigMap holds a metrics.yaml at
	// all; without one the collector uses the registry inside its image.
	hasMetricsFile bool
	metrics        renderedMetrics
}

func renderConfig(t *testing.T, merged MergedMetrics) renderedConfig {
	t.Helper()
	cm, _, err := BuildConfigMap(newTestMonitor(), merged)
	if err != nil {
		t.Fatalf("BuildConfigMap: %v", err)
	}
	var sources []pgwatchSource
	if err := yaml.Unmarshal([]byte(cm.Data[SourcesKey]), &sources); err != nil {
		t.Fatalf("sources.yaml: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("want one source, got %d", len(sources))
	}
	out := renderedConfig{source: sources[0]}
	rendered, ok := cm.Data[MetricsKey]
	out.hasMetricsFile = ok
	if err := yamlv3.Unmarshal([]byte(rendered), &out.metrics); err != nil {
		t.Fatalf("metrics.yaml: %v", err)
	}
	return out
}

// Whatever a source asks for has to be resolvable: from the registry inside
// the image when the monitor only names a preset, and from the metrics.yaml
// in the same ConfigMap when it has metrics of its own, because that file
// replaces the registry inside the image.
func TestBuildConfigMap_EveryPresetResolves(t *testing.T) {
	custom := MergedMetrics{
		Intervals:   map[string]int{"queue_depth": 45},
		Definitions: map[string]MetricDef{"queue_depth": {SQLs: map[int]string{14: "SELECT 1 AS depth"}}},
	}

	for _, name := range postgresPresets {
		t.Run(name+"/alone", func(t *testing.T) {
			cfg := renderConfig(t, MergedMetrics{Preset: name})
			if cfg.source.PresetMetrics != name || len(cfg.source.CustomMetrics) != 0 {
				t.Fatalf("source: preset_metrics=%q custom_metrics=%v", cfg.source.PresetMetrics, cfg.source.CustomMetrics)
			}
			if cfg.hasMetricsFile {
				t.Errorf("a monitor that only names a preset got a metrics.yaml: it would run on the operator's " +
					"copy of the registry instead of the one of its image")
			}
		})

		t.Run(name+"/with custom metrics", func(t *testing.T) {
			cfg := renderConfig(t, MergedMetrics{Preset: name, Intervals: custom.Intervals, Definitions: custom.Definitions})
			// pgwatch ignores custom_metrics when preset_metrics is set, so
			// the preset is spelled out instead.
			if cfg.source.PresetMetrics != "" {
				t.Fatalf("preset_metrics must be empty, got %q", cfg.source.PresetMetrics)
			}
			_, presets := builtinForTest(t)
			want := map[string]int{"queue_depth": 45}
			maps.Copy(want, presets[name].Metrics)
			if len(cfg.source.CustomMetrics) != len(want) {
				t.Errorf("custom_metrics has %d entries, want %d", len(cfg.source.CustomMetrics), len(want))
			}
			for metric, interval := range want {
				if got := cfg.source.CustomMetrics[metric]; got != interval {
					t.Errorf("custom_metrics[%q] = %d, want %d", metric, got, interval)
				}
				if _, ok := cfg.metrics.Metrics[metric]; !ok {
					t.Errorf("metrics.yaml does not define %q", metric)
				}
			}
		})
	}
}

// A preset the collector cannot resolve used to render fine when it stood
// alone: the pod ran, collected nothing, and the monitor looked healthy.
func TestBuildConfigMap_RefusesUnusablePresets(t *testing.T) {
	custom := MergedMetrics{
		Intervals:   map[string]int{"queue_depth": 45},
		Definitions: map[string]MetricDef{"queue_depth": {SQLs: map[int]string{14: "SELECT 1 AS depth"}}},
	}
	for _, name := range slices.Concat([]string{"exhaustiv"}, foreignPresets, poolerPresets) {
		for mode, merged := range map[string]MergedMetrics{
			"alone":               {Preset: name},
			"with custom metrics": {Preset: name, Intervals: custom.Intervals, Definitions: custom.Definitions},
		} {
			cm, hash, err := BuildConfigMap(newTestMonitor(), merged)
			if !errors.Is(err, ErrInvalidPreset) {
				t.Errorf("%s (%s): want ErrInvalidPreset, got %v", name, mode, err)
			}
			if cm != nil || hash != "" {
				t.Errorf("%s (%s): nothing must be rendered for an invalid preset", name, mode)
			}
		}
	}
}

// What changed in the presets between the pgwatch releases, as the upgrade
// notes describe it.
func TestBuiltinRegistry_ContentOfThisRelease(t *testing.T) {
	metrics, presets := builtinForTest(t)
	for _, metric := range []string{"settings_num", "autovacuum_scores", "recovery", "stat_lock", "instance_up"} {
		if _, ok := metrics[metric]; !ok {
			t.Errorf("metric %q is missing from the registry", metric)
		}
	}
	for _, name := range []string{"aiven", "aurora", "azure", "debug", "exhaustive", "exhaustive_no_python", "full", "gce", "rds"} {
		if _, ok := presets[name].Metrics["settings"]; ok {
			t.Errorf("preset %q still collects settings", name)
		}
		if _, ok := presets[name].Metrics["settings_num"]; !ok {
			t.Errorf("preset %q does not collect settings_num", name)
		}
	}
	// The docs promise these for the preset every example uses.
	for _, metric := range []string{"instance_up", "db_size", "db_stats", "wal"} {
		if _, ok := presets["basic"].Metrics[metric]; !ok {
			t.Errorf("preset basic does not collect %q", metric)
		}
	}
}

// The poolers' presets do have definitions, which is why they need a rule of
// their own: every one of their statements is an admin-console command.
func TestBuiltinRegistry_PoolerPresetsHoldNoSQLForPostgres(t *testing.T) {
	metrics, presets := builtinForTest(t)
	for _, name := range poolerPresets {
		preset, ok := presets[name]
		if !ok {
			t.Fatalf("preset %q left the registry: drop it from otherSourcePresets", name)
		}
		for metric := range preset.Metrics {
			var definition pgwatchMetric
			if err := metrics[metric].Decode(&definition); err != nil {
				t.Fatalf("metric %q: %v", metric, err)
			}
			for _, sql := range definition.SQLs {
				if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(sql)), "show ") {
					t.Errorf("preset %q, metric %q: %q is not an admin-console command; "+
						"is the preset usable for PostgreSQL after all?", name, metric, sql)
				}
			}
		}
	}
	for name := range otherSourcePresets {
		if !slices.Contains(poolerPresets, name) {
			t.Errorf("otherSourcePresets names %q, which this test does not know", name)
		}
	}
}

// The collector of a monitor with metrics of its own does not read the
// registry inside its image: it reads what the operator renders from its
// copy. Every built-in metric and preset has to arrive there as the file
// has it, with every key, also the ones pgwatch 6.2.0 declares and does not
// read yet (statement_timeout_seconds, is_private).
func TestRenderMetrics_KeepsTheRegistryIntact(t *testing.T) {
	type registry struct {
		Metrics map[string]map[string]any `yaml:"metrics"`
		Presets map[string]map[string]any `yaml:"presets"`
	}
	var embedded, rendered registry
	if err := yamlv3.Unmarshal(builtinMetricsYAML, &embedded); err != nil {
		t.Fatal(err)
	}
	out, err := renderMetrics(MergedMetrics{
		Preset:      "basic",
		Intervals:   map[string]int{"queue_depth": 45},
		Definitions: map[string]MetricDef{"queue_depth": {SQLs: map[int]string{14: "SELECT 1 AS depth"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := yamlv3.Unmarshal([]byte(out), &rendered); err != nil {
		t.Fatal(err)
	}

	if _, ok := rendered.Metrics["queue_depth"]; !ok {
		t.Errorf("the monitor's own metric is missing")
	}
	delete(rendered.Metrics, "queue_depth")
	if len(rendered.Metrics) != len(embedded.Metrics) || len(rendered.Presets) != len(embedded.Presets) {
		t.Fatalf("rendered %d built-in metrics and %d presets, the registry has %d and %d",
			len(rendered.Metrics), len(rendered.Presets), len(embedded.Metrics), len(embedded.Presets))
	}
	for name, want := range embedded.Metrics {
		if got := rendered.Metrics[name]; !reflect.DeepEqual(got, want) {
			t.Errorf("metric %q changed on its way through the operator:\n got  %v\n want %v", name, got, want)
		}
	}
	for name, want := range embedded.Presets {
		if got := rendered.Presets[name]; !reflect.DeepEqual(got, want) {
			t.Errorf("preset %q changed on its way through the operator:\n got  %v\n want %v", name, got, want)
		}
	}
	// The test above compares what it can see. That these keys are in the
	// file at all is what makes it a test of them.
	for metric, key := range map[string]string{
		"db_size": "statement_timeout_seconds", "table_stats": "statement_timeout_seconds", "reco_add_index": "is_private",
	} {
		if _, ok := rendered.Metrics[metric][key]; !ok {
			t.Errorf("metric %q arrives without %s", metric, key)
		}
	}
	// The same input gives the same bytes: the ConfigMap is hashed, and a
	// hash that moved would restart the collector.
	again, err := renderMetrics(MergedMetrics{
		Preset:      "basic",
		Intervals:   map[string]int{"queue_depth": 45},
		Definitions: map[string]MetricDef{"queue_depth": {SQLs: map[int]string{14: "SELECT 1 AS depth"}}},
	})
	if err != nil || again != out {
		t.Errorf("a second rendering differs from the first (%v)", err)
	}
}

// The registry is a quarter of a megabyte and is asked for several times in
// every reconcile of every monitor.
func TestLoadBuiltinRegistry_ParsesOnce(t *testing.T) {
	first, err := loadBuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadBuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("the embedded registry was parsed again")
	}
	if len(first.metrics) == 0 || len(first.presets) == 0 || len(first.presets) != len(first.presetNodes) {
		t.Errorf("%d metrics, %d presets, %d preset nodes", len(first.metrics), len(first.presets), len(first.presetNodes))
	}
}

func TestParseRegistry_SaysWhatIsWrong(t *testing.T) {
	for name, tc := range map[string]struct{ raw, want string }{
		"not YAML":                    {"metrics: [", "parse embedded pgwatch metrics.yaml"},
		"a preset that is no mapping": {"presets:\n  basic: [a, b]\n", `preset "basic"`},
		"an interval that is no number": {"presets:\n  basic:\n    metrics:\n      db_size: often\n",
			`preset "basic"`},
	} {
		if _, err := parseRegistry([]byte(tc.raw)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error with %q", name, err, tc.want)
		}
	}
	registry, err := parseRegistry([]byte("metrics:\n  a:\n    sqls:\n      14: SELECT 1\n"))
	if err != nil || len(registry.metrics) != 1 || len(registry.presets) != 0 {
		t.Errorf("a registry without presets: %+v, %v", registry, err)
	}
}

// Rendering hands the shared copy of the registry to the YAML encoder. It
// must come back as it went in, or the next monitor gets what the last one
// left of it.
func TestRenderMetrics_LeavesTheSharedRegistryAlone(t *testing.T) {
	registry, err := loadBuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	before, err := yamlv3.Marshal(registry.metrics)
	if err != nil {
		t.Fatal(err)
	}
	count := len(registry.metrics)
	override := MergedMetrics{
		Preset:      "basic",
		Intervals:   map[string]int{"db_size": 7, "queue_depth": 45},
		Definitions: map[string]MetricDef{"db_size": {SQLs: map[int]string{14: "SELECT 1 AS size_b"}}, "queue_depth": {}},
	}
	if _, err := renderMetrics(override); err != nil {
		t.Fatal(err)
	}
	after, err := yamlv3.Marshal(registry.metrics)
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.metrics) != count || string(after) != string(before) {
		t.Errorf("rendering a monitor that overrides db_size changed the registry for everyone")
	}
	plain, err := renderMetrics(MergedMetrics{Preset: "basic", Definitions: map[string]MetricDef{"queue_depth": {}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain, "SELECT 1 AS size_b") {
		t.Errorf("the override of one monitor shows in the metrics.yaml of another")
	}
}

// An interval the monitor gives for a metric of its preset wins over the
// preset's own, as docs/architecture.md says.
func TestBuildConfigMap_MonitorIntervalBeatsThePresets(t *testing.T) {
	_, presets := builtinForTest(t)
	if presets["basic"].Metrics["db_size"] == 7 {
		t.Fatal("the test needs an interval that differs from the preset's")
	}
	cfg := renderConfig(t, MergedMetrics{
		Preset:      "basic",
		Intervals:   map[string]int{"db_size": 7},
		Definitions: map[string]MetricDef{"db_size": {SQLs: map[int]string{14: "SELECT 1 AS size_b"}}},
	})
	if got := cfg.source.CustomMetrics["db_size"]; got != 7 {
		t.Errorf("custom_metrics[db_size] = %d, want the monitor's 7", got)
	}
	if got, want := cfg.source.CustomMetrics["wal"], presets["basic"].Metrics["wal"]; got != want {
		t.Errorf("custom_metrics[wal] = %d, want the preset's %d", got, want)
	}
	if got := cfg.metrics.Metrics["db_size"].SQLs[14]; got != "SELECT 1 AS size_b" {
		t.Errorf("the monitor's definition of db_size must replace the built-in one, got %q", got)
	}
}

// A definition that replaces a metric of the preset and names no interval
// keeps the preset's: changing the statement of a metric that runs every two
// hours is not a request to run it every minute.
func TestBuildConfigMap_AnOverrideWithoutIntervalKeepsThePresets(t *testing.T) {
	_, presets := builtinForTest(t)
	const slow = "table_bloat_approx_summary_sql"
	presetInterval := presets["exhaustive"].Metrics[slow]
	if presetInterval <= defaultInterval {
		t.Fatalf("the test needs a metric the preset collects less often than the default; %s has %d", slow, presetInterval)
	}

	statement := map[string]string{"14": "SELECT 1 AS approx_bloat_percentage"}
	merged := MergeMetrics(pgpilotv1.MetricsSpec{Preset: "exhaustive", Custom: []pgpilotv1.MetricDefinition{
		{Name: slow, SQLs: statement},
		{Name: "queue_depth", SQLs: statement},
		{Name: "db_size", SQLs: statement, Interval: &metav1.Duration{Duration: time.Minute}},
	}}, nil)
	cfg := renderConfig(t, merged)

	if got := cfg.source.CustomMetrics[slow]; got != presetInterval {
		t.Errorf("%s is collected every %d s, want the preset's %d", slow, got, presetInterval)
	}
	if got := cfg.metrics.Metrics[slow].SQLs[14]; got != statement["14"] {
		t.Errorf("the statement of %s is not the monitor's: %q", slow, got)
	}
	// A metric that is not in the preset has nothing to keep.
	if got := cfg.source.CustomMetrics["queue_depth"]; got != defaultInterval {
		t.Errorf("queue_depth is collected every %d s, want the default %d", got, defaultInterval)
	}
	// An interval that is named wins, also when it is the default's value.
	if presets["exhaustive"].Metrics["db_size"] == 60 {
		t.Fatal("the test needs a preset interval of db_size other than 60")
	}
	if got := cfg.source.CustomMetrics["db_size"]; got != 60 {
		t.Errorf("db_size is collected every %d s, want the 60 the monitor names", got)
	}

	// Without a preset there is nothing to keep either.
	alone := renderConfig(t, MergeMetrics(pgpilotv1.MetricsSpec{Custom: []pgpilotv1.MetricDefinition{
		{Name: slow, SQLs: statement},
	}}, nil))
	if got := alone.source.CustomMetrics[slow]; got != defaultInterval {
		t.Errorf("without a preset %s is collected every %d s, want the default %d", slow, got, defaultInterval)
	}
}

func TestHasDefinedMetric(t *testing.T) {
	metrics := map[string]*yamlv3.Node{"a": {}, "b": {}}
	cases := map[string]struct {
		preset map[string]int
		want   bool
	}{
		"all defined":  {map[string]int{"a": 1, "b": 1}, true},
		"one defined":  {map[string]int{"a": 1, "x": 1}, true},
		"none defined": {map[string]int{"x": 1, "y": 1}, false},
		"empty":        {nil, false},
	}
	for name, tc := range cases {
		if got := hasDefinedMetric(pgwatchPreset{Metrics: tc.preset}, metrics); got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}

var presetInBackticks = regexp.MustCompile("`([a-z][a-z0-9_-]*)`")

// The docs list the presets by name; a list that drifts from the registry
// sends users to InvalidPreset.
func TestDocs_NameOnlyPresetsThatExist(t *testing.T) {
	usable, err := Presets()
	if err != nil {
		t.Fatal(err)
	}

	reference, err := os.ReadFile(filepath.Join("..", "..", "docs", "crd-reference.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, after, found := strings.Cut(string(reference), "The presets of pgwatch "+PgwatchVersion+" that a monitor can use:")
	if !found {
		t.Fatal("docs/crd-reference.md no longer introduces the preset list for pgwatch " + PgwatchVersion)
	}
	sentence, _, _ := strings.Cut(after, ". ")
	names := presetInBackticks.FindAllStringSubmatch(sentence, -1)
	listed := make([]string, 0, len(names))
	for _, m := range names {
		listed = append(listed, m[1])
	}
	slices.Sort(listed)
	if !slices.Equal(listed, usable) {
		t.Errorf("docs/crd-reference.md lists the presets\n     %v\nwant %v", listed, usable)
	}

	observability, err := os.ReadFile(filepath.Join("..", "..", "docs", "observability.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, table, found := strings.Cut(string(observability), "| Preset | Metrics | Collects |")
	if !found {
		t.Fatal("docs/observability.md no longer has the preset table")
	}
	table, _, _ = strings.Cut(table, "\n\n")
	_, presets := builtinForTest(t)
	var inTable []string
	for _, row := range strings.Split(strings.TrimSpace(table), "\n")[1:] {
		cells := strings.Split(row, "|")
		if len(cells) < 4 {
			t.Fatalf("unexpected row in the preset table: %q", row)
		}
		names := presetInBackticks.FindAllStringSubmatch(cells[1], -1)
		for _, m := range names {
			inTable = append(inTable, m[1])
		}
		// A row for a single preset states how many metrics it has.
		if len(names) == 1 {
			if want := strconv.Itoa(len(presets[names[0][1]].Metrics)); strings.TrimSpace(cells[2]) != want {
				t.Errorf("docs/observability.md says preset %s has %s metrics, the registry has %s",
					names[0][1], strings.TrimSpace(cells[2]), want)
			}
		}
	}
	slices.Sort(inTable)
	if !slices.Equal(inTable, usable) {
		t.Errorf("the preset table of docs/observability.md has\n     %v\nwant %v", inTable, usable)
	}
}

// The monitor's own definitions are written through pgwatchMetric. The other
// tests read them back through the same struct, so a key that is renamed
// there is renamed on both sides and nothing notices. Before the built-in
// definitions were handed on as nodes, TestRenderMetrics_KeepsTheRegistryIntact
// pinned these keys, because the registry went through the struct too.
func TestRenderMetrics_OwnDefinitionIsWrittenWithTheKeysPgwatchReads(t *testing.T) {
	out, err := renderMetrics(MergedMetrics{Definitions: map[string]MetricDef{"queue_depth": {
		SQLs:            map[int]string{14: "SELECT 1 AS depth"},
		Gauges:          []string{"*"},
		IsInstanceLevel: true,
		NodeStatus:      "primary",
		Description:     "depth of the queue",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	var rendered struct {
		Metrics map[string]map[string]any `yaml:"metrics"`
	}
	if err := yamlv3.Unmarshal([]byte(out), &rendered); err != nil {
		t.Fatal(err)
	}
	// The names are those of the yaml tags of pgwatch's metrics.Metric
	// (internal/metrics/types.go), and of the keys in the embedded registry.
	want := map[string]any{
		"sqls":              map[any]any{14: "SELECT 1 AS depth"},
		"gauges":            []any{"*"},
		"is_instance_level": true,
		"node_status":       "primary",
		"description":       "depth of the queue",
	}
	if got := rendered.Metrics["queue_depth"]; !reflect.DeepEqual(got, want) {
		t.Errorf("the monitor's own metric is written as\n got  %#v\n want %#v", got, want)
	}

	// And every key the operator writes is one the registry itself uses.
	registry, err := loadBuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for _, node := range registry.metrics {
		var definition map[string]any
		if err := node.Decode(&definition); err != nil {
			t.Fatal(err)
		}
		for key := range definition {
			known[key] = true
		}
	}
	for key := range want {
		if !known[key] {
			t.Errorf("key %q is in no built-in definition: is it a key pgwatch reads?", key)
		}
	}
}

// A definition without an interval keeps the preset's also when the preset
// collects the metric MORE often than the default (stat_activity: 30 s in
// "exhaustive"), and a named interval wins also when it is LONGER than the
// preset's. The existing test only has a preset interval above 60 with an
// unnamed one, and named intervals below the preset's.
func TestExpandPreset_IntervalRuleInBothDirections(t *testing.T) {
	registry, err := loadBuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	preset := registry.presets["exhaustive"].Metrics
	const fast, slowed = "stat_activity", "db_size"
	if preset[fast] >= defaultInterval {
		t.Fatalf("the test needs a metric the preset collects more often than every %d s; %s has %d",
			defaultInterval, fast, preset[fast])
	}
	named := 2 * preset[slowed]
	statement := map[string]string{"14": "SELECT 1 AS v"}
	merged := MergeMetrics(pgpilotv1.MetricsSpec{Preset: "exhaustive", Custom: []pgpilotv1.MetricDefinition{
		{Name: fast, SQLs: statement},
		{Name: slowed, SQLs: statement, Interval: &metav1.Duration{Duration: time.Duration(named) * time.Second}},
	}}, nil)
	got, err := expandPresetIntoIntervals(merged)
	if err != nil {
		t.Fatal(err)
	}
	if got[fast] != preset[fast] {
		t.Errorf("%s names no interval and is collected every %d s, want the preset's %d", fast, got[fast], preset[fast])
	}
	if got[slowed] != named {
		t.Errorf("%s names %d s and is collected every %d s", slowed, named, got[slowed])
	}
}

// A definition that comes from a library and names no interval keeps the
// preset's too. TestMergeMetrics_RemembersWhichIntervalsAreNotSet has no
// metric that only a library defines without an interval, and the test of
// the rule itself uses inline definitions only.
func TestMergeMetrics_ALibraryDefinitionWithoutIntervalKeepsThePresets(t *testing.T) {
	const slow = "table_bloat_approx_summary_sql"
	library := pgpilotv1.PgpilotMetricLibrary{}
	library.Spec.Metrics = []pgpilotv1.MetricDefinition{{Name: slow, SQLs: map[string]string{"14": "SELECT 1 AS v"}}}
	merged := MergeMetrics(pgpilotv1.MetricsSpec{
		Preset: "exhaustive",
		Custom: []pgpilotv1.MetricDefinition{{Name: "queue_depth", SQLs: map[string]string{"14": "SELECT 1 AS v"}}},
	}, []pgpilotv1.PgpilotMetricLibrary{library})
	if !merged.IntervalUnset[slow] {
		t.Errorf("the library's %s names no interval and is not marked: %v", slow, merged.IntervalUnset)
	}
	registry, err := loadBuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	got, err := expandPresetIntoIntervals(merged)
	if err != nil {
		t.Fatal(err)
	}
	if want := registry.presets["exhaustive"].Metrics[slow]; got[slow] != want {
		t.Errorf("%s from a library is collected every %d s, want the preset's %d", slow, got[slow], want)
	}
}

// expandPresetIntoIntervals reads the shared registry's presets. What one
// monitor adds or overrides must not show in the preset of the next one.
// TestRenderMetrics_LeavesTheSharedRegistryAlone looks at registry.metrics
// only, and only around renderMetrics.
func TestExpandPreset_LeavesTheSharedPresetsAlone(t *testing.T) {
	registry, err := loadBuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	before := make(map[string]map[string]int, len(registry.presets))
	for name, preset := range registry.presets {
		before[name] = maps.Clone(preset.Metrics)
	}
	nodesBefore, err := yamlv3.Marshal(registry.presetNodes)
	if err != nil {
		t.Fatal(err)
	}

	merged := MergedMetrics{
		Preset:      "basic",
		Intervals:   map[string]int{"db_size": 7, "queue_depth": 45},
		Definitions: map[string]MetricDef{"db_size": {SQLs: map[int]string{14: "SELECT 1 AS size_b"}}, "queue_depth": {}},
	}
	if _, _, err := BuildConfigMap(newTestMonitor(), merged); err != nil {
		t.Fatal(err)
	}

	for name, preset := range registry.presets {
		if !maps.Equal(preset.Metrics, before[name]) {
			t.Errorf("preset %q of the shared registry changed:\n got  %v\n want %v", name, preset.Metrics, before[name])
		}
	}
	nodesAfter, err := yamlv3.Marshal(registry.presetNodes)
	if err != nil {
		t.Fatal(err)
	}
	if string(nodesAfter) != string(nodesBefore) {
		t.Errorf("the preset nodes of the shared registry changed")
	}
	// The next monitor: the preset as the registry has it, and its own metric.
	next, err := expandPresetIntoIntervals(MergedMetrics{Preset: "basic", Intervals: map[string]int{"other": 10}})
	if err != nil {
		t.Fatal(err)
	}
	want := maps.Clone(before["basic"])
	want["other"] = 10
	if !maps.Equal(next, want) {
		t.Errorf("the next monitor on the preset collects %v, want %v", next, want)
	}
}

// The registry is shared by every reconcile. Run with -race: the encoder is
// handed the same nodes from several goroutines.
func TestBuildConfigMap_ConcurrentRendersShareTheRegistry(t *testing.T) {
	merged := MergedMetrics{
		Preset:      "exhaustive",
		Intervals:   map[string]int{"queue_depth": 45},
		Definitions: map[string]MetricDef{"queue_depth": {SQLs: map[int]string{14: "SELECT 1 AS depth"}}},
	}
	first, hash, err := BuildConfigMap(newTestMonitor(), merged)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			cm, again, err := BuildConfigMap(newTestMonitor(), merged)
			if err != nil || again != hash || cm.Data[MetricsKey] != first.Data[MetricsKey] {
				t.Errorf("a concurrent rendering differs (%v)", err)
			}
		})
	}
	wg.Wait()
}

// A definition without an interval that carries the name of a built-in
// metric which is NOT in the monitor's preset has no interval to keep: it is
// collected at the default, like any other metric of the monitor's own. With
// the rule written against the registry instead of the preset
// (registry.metrics[name] in place of preset.Metrics[name]) such a metric
// drops out of custom_metrics and is never collected.
func TestExpandPreset_ABuiltinNameOutsideThePresetIsCollectedAtTheDefault(t *testing.T) {
	registry, err := loadBuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	const outside = "table_stats"
	if _, builtin := registry.metrics[outside]; !builtin {
		t.Fatalf("the test needs a built-in metric; %s is none", outside)
	}
	if _, in := registry.presets["basic"].Metrics[outside]; in {
		t.Fatalf("the test needs a built-in metric that preset basic does not collect; it collects %s", outside)
	}
	merged := MergeMetrics(pgpilotv1.MetricsSpec{Preset: "basic", Custom: []pgpilotv1.MetricDefinition{
		{Name: outside, SQLs: map[string]string{"14": "SELECT 1 AS v"}},
	}}, nil)
	got, err := expandPresetIntoIntervals(merged)
	if err != nil {
		t.Fatal(err)
	}
	if got[outside] != defaultInterval {
		t.Errorf("%s is collected every %d s, want the default %d: %v", outside, got[outside], defaultInterval, got)
	}
}

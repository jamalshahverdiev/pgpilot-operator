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
	"testing"

	yamlv3 "gopkg.in/yaml.v3"
	"sigs.k8s.io/yaml"
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

	_, all, err := loadBuiltinMetrics()
	if err != nil {
		t.Fatal(err)
	}
	if want := len(postgresPresets) + len(foreignPresets) + len(poolerPresets); len(all) != want {
		t.Errorf("the registry has %d presets, the test knows %d: classify the new ones", len(all), want)
	}
}

// Every metric of a usable preset needs a definition, or the collector logs
// "metric definition not found" for it and the preset is silently smaller
// than its name promises.
func TestBuiltinRegistry_PresetsAreFullyDefined(t *testing.T) {
	metrics, presets, err := loadBuiltinMetrics()
	if err != nil {
		t.Fatal(err)
	}
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

// renderedConfig is what the collector reads back from the ConfigMap.
type renderedConfig struct {
	source  pgwatchSource
	metrics pgwatchMetricsFile
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
	if err := yamlv3.Unmarshal([]byte(cm.Data[MetricsKey]), &out.metrics); err != nil {
		t.Fatalf("metrics.yaml: %v", err)
	}
	return out
}

// Whatever a source asks for has to be resolvable from the metrics.yaml in
// the same ConfigMap: that file replaces the registry inside the image.
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
			preset, ok := cfg.metrics.Presets[name]
			if !ok {
				t.Fatalf("metrics.yaml has no preset %q", name)
			}
			for metric := range preset.Metrics {
				if _, ok := cfg.metrics.Metrics[metric]; !ok {
					t.Errorf("metrics.yaml does not define %q", metric)
				}
			}
		})

		t.Run(name+"/with custom metrics", func(t *testing.T) {
			cfg := renderConfig(t, MergedMetrics{Preset: name, Intervals: custom.Intervals, Definitions: custom.Definitions})
			// pgwatch ignores custom_metrics when preset_metrics is set, so
			// the preset is spelled out instead.
			if cfg.source.PresetMetrics != "" {
				t.Fatalf("preset_metrics must be empty, got %q", cfg.source.PresetMetrics)
			}
			_, presets, err := loadBuiltinMetrics()
			if err != nil {
				t.Fatal(err)
			}
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
	metrics, presets, err := loadBuiltinMetrics()
	if err != nil {
		t.Fatal(err)
	}
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
	metrics, presets, err := loadBuiltinMetrics()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range poolerPresets {
		preset, ok := presets[name]
		if !ok {
			t.Fatalf("preset %q left the registry: drop it from otherSourcePresets", name)
		}
		for metric := range preset.Metrics {
			for _, sql := range metrics[metric].SQLs {
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

// droppedByTheOperator are keys of a metric definition that the operator's
// structs do not carry. pgwatch 6.2.0 declares both and reads neither; a key
// that is not listed here must survive the trip through the operator.
var droppedByTheOperator = []string{"statement_timeout_seconds", "is_private"}

// The collector does not read the embedded file: it reads what the operator
// renders from it. Everything pgwatch acts on has to arrive unchanged.
func TestRenderMetrics_KeepsTheRegistryIntact(t *testing.T) {
	type registry struct {
		Metrics map[string]map[string]any `yaml:"metrics"`
		Presets map[string]map[string]any `yaml:"presets"`
	}
	var embedded, rendered registry
	if err := yamlv3.Unmarshal(builtinMetricsYAML, &embedded); err != nil {
		t.Fatal(err)
	}
	out, err := renderMetrics(MergedMetrics{Preset: "basic"})
	if err != nil {
		t.Fatal(err)
	}
	if err := yamlv3.Unmarshal([]byte(out), &rendered); err != nil {
		t.Fatal(err)
	}

	if len(rendered.Metrics) != len(embedded.Metrics) || len(rendered.Presets) != len(embedded.Presets) {
		t.Fatalf("rendered %d metrics and %d presets, the registry has %d and %d",
			len(rendered.Metrics), len(rendered.Presets), len(embedded.Metrics), len(embedded.Presets))
	}
	for name, want := range embedded.Metrics {
		for _, key := range droppedByTheOperator {
			delete(want, key)
		}
		// An empty value and an absent key mean the same to pgwatch.
		for key, value := range want {
			if value == nil || value == "" || value == false {
				delete(want, key)
			}
		}
		if got := rendered.Metrics[name]; !reflect.DeepEqual(got, want) {
			t.Errorf("metric %q changed on its way through the operator:\n got  %v\n want %v", name, got, want)
		}
	}
	for name, want := range embedded.Presets {
		if got := rendered.Presets[name]; !reflect.DeepEqual(got, want) {
			t.Errorf("preset %q changed on its way through the operator:\n got  %v\n want %v", name, got, want)
		}
	}
}

// An interval the monitor gives for a metric of its preset wins over the
// preset's own, as docs/architecture.md says.
func TestBuildConfigMap_MonitorIntervalBeatsThePresets(t *testing.T) {
	_, presets, err := loadBuiltinMetrics()
	if err != nil {
		t.Fatal(err)
	}
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

func TestHasDefinedMetric(t *testing.T) {
	metrics := map[string]pgwatchMetric{"a": {}, "b": {}}
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
	_, presets, err := loadBuiltinMetrics()
	if err != nil {
		t.Fatal(err)
	}
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

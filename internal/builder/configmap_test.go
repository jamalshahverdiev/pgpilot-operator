package builder

import (
	"maps"
	"strings"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"
	"sigs.k8s.io/yaml"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
)

const (
	testMonitorName    = "test-db"
	testMonitorNS      = "team-test"
	testDeploymentName = "pgpilot-test-db"
)

func newTestMonitor() *pgpilotv1.PgpilotMonitor {
	return &pgpilotv1.PgpilotMonitor{
		Name:      testMonitorName,
		Namespace: testMonitorNS,
		Spec: pgpilotv1.PgpilotMonitorSpec{
			Database: pgpilotv1.DatabaseRef{
				Host:     "pg.example.com",
				Port:     5432,
				Database: "mydb",
				SSLMode:  pgpilotv1.SSLModeRequire,
				CredentialsSecret: &pgpilotv1.CredentialsSecretRef{
					Name: "pg-creds",
				},
				CustomTags: map[string]string{"env": "test"},
			},
			Sinks: pgpilotv1.SinksSpec{
				Prometheus: &pgpilotv1.PrometheusSink{
					Enabled: true,
					Port:    9187,
				},
			},
		},
	}
}

func TestBuildConfigMap_SourcesYAML_ContainsConnStr(t *testing.T) {
	monitor := newTestMonitor()
	merged := MergedMetrics{Preset: "exhaustive"}

	cm, _, err := BuildConfigMap(monitor, merged)
	if err != nil {
		t.Fatalf("BuildConfigMap: %v", err)
	}

	sources := cm.Data[SourcesKey]
	if !strings.Contains(sources, "pg.example.com:5432") {
		t.Errorf("sources.yaml missing host:port, got:\n%s", sources)
	}
	if strings.Contains(sources, "PGUSER") || strings.Contains(sources, "PGPASSWORD") {
		t.Error("sources.yaml should not contain credentials — they are passed via PGUSER/PGPASSWORD env vars")
	}
	if !strings.Contains(sources, "sslmode=require") {
		t.Errorf("sources.yaml missing sslmode, got:\n%s", sources)
	}
	if !strings.Contains(sources, "preset_metrics: exhaustive") {
		t.Errorf("sources.yaml missing preset, got:\n%s", sources)
	}
}

func TestBuildConfigMap_NoMetricsYAML_WhenNoPresetAndNoCustom(t *testing.T) {
	monitor := newTestMonitor()
	merged := MergedMetrics{}

	cm, _, err := BuildConfigMap(monitor, merged)
	if err != nil {
		t.Fatalf("BuildConfigMap: %v", err)
	}

	if _, ok := cm.Data[MetricsKey]; ok {
		t.Error("metrics.yaml should not be present when no preset and no custom definitions")
	}
}

// A monitor that only names a preset runs on the registry inside its image:
// the definitions that belong to the pgwatch release it actually runs. A
// metrics.yaml would put the operator's copy in their place.
func TestBuildConfigMap_NoMetricsYAML_WhenPresetOnly(t *testing.T) {
	monitor := newTestMonitor()
	merged := MergedMetrics{Preset: "basic"}

	cm, _, err := BuildConfigMap(monitor, merged)
	if err != nil {
		t.Fatalf("BuildConfigMap: %v", err)
	}
	if len(cm.Data) != 1 {
		t.Errorf("the ConfigMap holds %d keys, want sources.yaml alone", len(cm.Data))
	}
	if metricsYAML, ok := cm.Data[MetricsKey]; ok {
		t.Errorf("metrics.yaml of %d bytes for a monitor without metrics of its own", len(metricsYAML))
	}
	if !strings.Contains(cm.Data[SourcesKey], "preset_metrics: basic") {
		t.Errorf("sources.yaml does not name the preset:\n%s", cm.Data[SourcesKey])
	}
	// A quarter of a megabyte per monitor, in the API server and in the
	// operator's cache, was the registry.
	if size := len(cm.Data[SourcesKey]); size > 1024 {
		t.Errorf("sources.yaml alone has %d bytes", size)
	}
}

func TestNeedsMetricsFile(t *testing.T) {
	definition := map[string]MetricDef{"queue_depth": {SQLs: map[int]string{14: "SELECT 1"}}}
	for name, tc := range map[string]struct {
		merged MergedMetrics
		want   bool
	}{
		"nothing":                   {MergedMetrics{}, false},
		"a preset":                  {MergedMetrics{Preset: "basic"}, false},
		"metrics of its own":        {MergedMetrics{Definitions: definition}, true},
		"a preset and its own":      {MergedMetrics{Preset: "basic", Definitions: definition}, true},
		"an empty set of its own":   {MergedMetrics{Preset: "basic", Definitions: map[string]MetricDef{}}, false},
		"intervals, no definitions": {MergedMetrics{Preset: "basic", Intervals: map[string]int{"x": 1}}, false},
	} {
		if got := NeedsMetricsFile(tc.merged); got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}

// Two monitors of one name in two namespaces are one "dbname" to whatever
// receives their measurements. The namespace is added as a tag, and it is
// the operator's to set.
func TestBuildConfigMap_NamespaceTag(t *testing.T) {
	tagsOf := func(monitor *pgpilotv1.PgpilotMonitor) map[string]string {
		t.Helper()
		cm, _, err := BuildConfigMap(monitor, MergedMetrics{Preset: "basic"})
		if err != nil {
			t.Fatalf("BuildConfigMap: %v", err)
		}
		var sources []pgwatchSource
		if err := yaml.Unmarshal([]byte(cm.Data[SourcesKey]), &sources); err != nil || len(sources) != 1 {
			t.Fatalf("sources.yaml: %v\n%s", err, cm.Data[SourcesKey])
		}
		if sources[0].Name != monitor.Name {
			t.Errorf("the source is called %q; the dbname label must stay the monitor's name, %q", sources[0].Name, monitor.Name)
		}
		return sources[0].CustomTags
	}

	t.Run("next to the monitor's own tags", func(t *testing.T) {
		monitor := newTestMonitor()
		monitor.Spec.Database.CustomTags = map[string]string{"team": "users", "env": "prod"}
		want := map[string]string{"team": "users", "env": "prod", "namespace": testMonitorNS}
		if got := tagsOf(monitor); !maps.Equal(got, want) {
			t.Errorf("custom_tags %v, want %v", got, want)
		}
		// The monitor's own map is its spec: it must not gain the tag.
		if _, added := monitor.Spec.Database.CustomTags["namespace"]; added || len(monitor.Spec.Database.CustomTags) != 2 {
			t.Errorf("the monitor's customTags were changed: %v", monitor.Spec.Database.CustomTags)
		}
	})

	t.Run("for a monitor without tags", func(t *testing.T) {
		monitor := newTestMonitor()
		monitor.Spec.Database.CustomTags = nil
		if got := tagsOf(monitor); !maps.Equal(got, map[string]string{"namespace": testMonitorNS}) {
			t.Errorf("custom_tags %v", got)
		}
	})

	t.Run("not the namespace a monitor claims", func(t *testing.T) {
		monitor := newTestMonitor()
		monitor.Spec.Database.CustomTags = map[string]string{"namespace": "somebody-elses", "env": "prod"}
		if got := tagsOf(monitor); got["namespace"] != testMonitorNS || got["env"] != "prod" {
			t.Errorf("custom_tags %v, want namespace %q", got, testMonitorNS)
		}
	})

	t.Run("each monitor its own", func(t *testing.T) {
		a, b := newTestMonitor(), newTestMonitor()
		b.Namespace = "another"
		if tagsOf(a)["namespace"] == tagsOf(b)["namespace"] {
			t.Errorf("two namespaces, one tag")
		}
	})
}

func TestBuildConfigMap_MetricsYAML_PresetPlusCustom(t *testing.T) {
	monitor := newTestMonitor()
	merged := MergedMetrics{
		Preset:    "exhaustive",
		Intervals: map[string]int{"my_metric": 30},
		Definitions: map[string]MetricDef{
			"my_metric": {SQLs: map[int]string{13: "SELECT 1"}},
		},
	}

	cm, _, err := BuildConfigMap(monitor, merged)
	if err != nil {
		t.Fatalf("BuildConfigMap: %v", err)
	}

	metricsYAML, ok := cm.Data[MetricsKey]
	if !ok {
		t.Fatal("metrics.yaml should be present")
	}
	if !strings.Contains(metricsYAML, "my_metric") {
		t.Error("custom metric missing")
	}
	if !strings.Contains(metricsYAML, "db_stats") {
		t.Error("built-in metric missing when preset is set alongside custom")
	}
}

func TestBuildConfigMap_MetricsYAML_WhenCustomPresent(t *testing.T) {
	monitor := newTestMonitor()
	merged := MergedMetrics{
		Intervals: map[string]int{"my_metric": 30},
		Definitions: map[string]MetricDef{
			"my_metric": {
				SQLs:   map[int]string{13: "SELECT 1"},
				Gauges: []string{"*"},
			},
		},
	}

	cm, _, err := BuildConfigMap(monitor, merged)
	if err != nil {
		t.Fatalf("BuildConfigMap: %v", err)
	}

	metricsYAML, ok := cm.Data[MetricsKey]
	if !ok {
		t.Fatal("metrics.yaml should be present when custom definitions exist")
	}
	if !strings.Contains(metricsYAML, "my_metric") {
		t.Errorf("metrics.yaml missing metric name, got:\n%s", metricsYAML)
	}
	// Without a preset there is nothing of the registry to hand on: the
	// file holds the monitor's metrics and nothing else.
	var rendered struct {
		Metrics map[string]any `yaml:"metrics"`
		Presets map[string]any `yaml:"presets"`
	}
	if err := yamlv3.Unmarshal([]byte(metricsYAML), &rendered); err != nil {
		t.Fatal(err)
	}
	if len(rendered.Metrics) != 1 || len(rendered.Presets) != 0 {
		t.Errorf("metrics.yaml holds %d metrics and %d presets, want the monitor's one metric", len(rendered.Metrics), len(rendered.Presets))
	}
}

func TestBuildConfigMap_HashStability(t *testing.T) {
	monitor := newTestMonitor()
	merged := MergedMetrics{Preset: "basic"}

	_, hash1, _ := BuildConfigMap(monitor, merged)
	_, hash2, _ := BuildConfigMap(monitor, merged)

	if hash1 != hash2 {
		t.Errorf("hash not stable: %q != %q", hash1, hash2)
	}
}

func TestBuildConfigMap_HashChanges_OnSpecChange(t *testing.T) {
	monitor := newTestMonitor()
	merged1 := MergedMetrics{Preset: "basic"}
	merged2 := MergedMetrics{Preset: "exhaustive"}

	_, hash1, _ := BuildConfigMap(monitor, merged1)
	_, hash2, _ := BuildConfigMap(monitor, merged2)

	if hash1 == hash2 {
		t.Error("hash should change when preset changes")
	}
}

func TestBuildConfigMap_CustomTags(t *testing.T) {
	monitor := newTestMonitor()
	monitor.Spec.Database.CustomTags = map[string]string{"team": "users", "env": "prod"}
	merged := MergedMetrics{Preset: "basic"}

	cm, _, err := BuildConfigMap(monitor, merged)
	if err != nil {
		t.Fatalf("BuildConfigMap: %v", err)
	}

	sources := cm.Data[SourcesKey]
	if !strings.Contains(sources, "team: users") {
		t.Errorf("sources.yaml missing custom_tags, got:\n%s", sources)
	}
}

func TestBuildConfigMap_Labels(t *testing.T) {
	monitor := newTestMonitor()
	merged := MergedMetrics{Preset: "basic"}

	cm, _, _ := BuildConfigMap(monitor, merged)

	if cm.Labels[LabelMonitorName] != testMonitorName {
		t.Errorf("label %s: got %q, want %q", LabelMonitorName, cm.Labels[LabelMonitorName], testMonitorName)
	}
	if cm.Name != "pgpilot-test-db-config" {
		t.Errorf("name: got %q, want %q", cm.Name, "pgpilot-test-db-config")
	}
	if cm.Namespace != testMonitorNS {
		t.Errorf("namespace: got %q, want %q", cm.Namespace, testMonitorNS)
	}
}

func TestBuildConfigMap_SourcesCustomMetrics(t *testing.T) {
	monitor := newTestMonitor()
	merged := MergedMetrics{
		Intervals: map[string]int{"my_metric": 45},
		Definitions: map[string]MetricDef{
			"my_metric": {SQLs: map[int]string{13: "SELECT 1"}},
		},
	}

	cm, _, err := BuildConfigMap(monitor, merged)
	if err != nil {
		t.Fatalf("BuildConfigMap: %v", err)
	}

	sources := cm.Data[SourcesKey]
	if !strings.Contains(sources, "my_metric: 45") {
		t.Errorf("sources.yaml missing custom_metrics interval, got:\n%s", sources)
	}
}

// silence unused import
var _ = time.Second

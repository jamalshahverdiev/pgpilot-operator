package builder

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	yamlv3 "gopkg.in/yaml.v3"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
)

const (
	SourcesKey = "sources.yaml"
	MetricsKey = "metrics.yaml"
)

// BuildConfigMap creates the ConfigMap that pgwatch mounts as its
// configuration. It contains sources.yaml (connection + intervals) and
// optionally metrics.yaml (custom SQL definitions).
//
// The returned string is the SHA-256 hash of the combined data, used as
// the pgpilot.io/config-hash annotation on the Deployment pod template to
// trigger rollouts on config changes.
func BuildConfigMap(
	monitor *pgpilotv1.PgpilotMonitor,
	merged MergedMetrics,
) (*corev1.ConfigMap, string, error) {
	sourcesYAML, err := renderSources(monitor, merged)
	if err != nil {
		return nil, "", fmt.Errorf("render sources.yaml: %w", err)
	}

	data := map[string]string{
		SourcesKey: sourcesYAML,
	}

	// We emit metrics.yaml when the user has a preset (so we can ship the
	// full built-in registry alongside any custom metrics) or when they
	// have custom definitions to inject.
	if merged.Preset != "" || len(merged.Definitions) > 0 {
		metricsYAML, err := renderMetrics(merged)
		if err != nil {
			return nil, "", fmt.Errorf("render metrics.yaml: %w", err)
		}
		data[MetricsKey] = metricsYAML
	}

	hash := contentHash(data)

	cm := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ResourceName(monitor.Name) + "-config",
			Namespace: monitor.Namespace,
			Labels:    SystemLabels(monitor.Name),
		},
		Data: data,
	}

	return cm, hash, nil
}

// pgwatch source entry — matches internal/sources.Source YAML tags.
type pgwatchSource struct {
	Name          string            `json:"name"`
	ConnStr       string            `json:"conn_str"`
	Kind          string            `json:"kind"`
	PresetMetrics string            `json:"preset_metrics,omitempty"`
	CustomMetrics map[string]int    `json:"custom_metrics,omitempty"`
	IsEnabled     bool              `json:"is_enabled"`
	CustomTags    map[string]string `json:"custom_tags,omitempty"`
}

func renderSources(monitor *pgpilotv1.PgpilotMonitor, merged MergedMetrics) (string, error) {
	db := monitor.Spec.Database

	connStr := buildConnStr(db)

	src := pgwatchSource{
		Name:       monitor.Name,
		ConnStr:    connStr,
		Kind:       "postgres",
		IsEnabled:  true,
		CustomTags: db.CustomTags,
	}

	// pgwatch's source resolution logic sets md.Metrics = GetPresetMetrics(preset)
	// when PresetMetrics is set, fully REPLACING any custom_metrics. That means
	// "preset + custom_metrics" in the same source never works as addition.
	//
	// To support preset + custom together we expand the preset ourselves into
	// custom_metrics (preset's metric intervals + user's custom intervals) and
	// skip preset_metrics. When only preset is set, we use preset_metrics as-is
	// so pgwatch can use its embedded metrics.yaml without extra work.
	switch {
	case merged.Preset != "" && len(merged.Intervals) > 0:
		expanded, err := expandPresetIntoIntervals(merged.Preset, merged.Intervals)
		if err != nil {
			return "", err
		}
		src.CustomMetrics = expanded
	case merged.Preset != "":
		src.PresetMetrics = merged.Preset
	case len(merged.Intervals) > 0:
		src.CustomMetrics = merged.Intervals
	}

	out, err := yaml.Marshal([]pgwatchSource{src})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func buildConnStr(db pgpilotv1.DatabaseRef) string {
	port := db.Port
	if port == 0 {
		port = 5432
	}
	sslmode := db.SSLMode
	if sslmode == "" {
		sslmode = pgpilotv1.SSLModeDisable
	}
	return fmt.Sprintf(
		"postgresql://%s:%d/%s?sslmode=%s",
		db.Host, port, db.Database, sslmode,
	)
}

// pgwatch metrics.yaml top-level structure.
// Uses yaml tags (not json) because renderMetrics uses gopkg.in/yaml.v3 directly.
type pgwatchMetricsFile struct {
	Metrics map[string]pgwatchMetric `yaml:"metrics,omitempty"`
	Presets map[string]pgwatchPreset `yaml:"presets,omitempty"`
}

type pgwatchMetric struct {
	InitSQL         string         `yaml:"init_sql,omitempty"`
	SQLs            map[int]string `yaml:"sqls"`
	Gauges          []string       `yaml:"gauges,omitempty"`
	IsInstanceLevel bool           `yaml:"is_instance_level,omitempty"`
	NodeStatus      string         `yaml:"node_status,omitempty"`
	StorageName     string         `yaml:"storage_name,omitempty"`
	Description     string         `yaml:"description,omitempty"`
}

type pgwatchPreset struct {
	Description string         `yaml:"description,omitempty"`
	Metrics     map[string]int `yaml:"metrics"`
}

// renderMetrics builds the pgwatch metrics.yaml. When a preset is selected
// alongside custom metrics, we must ship pgwatch's entire built-in metric +
// preset registry because the --metrics flag REPLACES rather than merges with
// pgwatch's embedded defaults. The embedded builtin covers this.
func renderMetrics(merged MergedMetrics) (string, error) {
	metrics := map[string]pgwatchMetric{}
	presets := map[string]pgwatchPreset{}

	// Start from the embedded pgwatch built-in registry when a preset is
	// requested — otherwise the preset would resolve to no metrics.
	if merged.Preset != "" {
		bm, bp, err := loadBuiltinMetrics()
		if err != nil {
			return "", err
		}
		maps.Copy(metrics, bm)
		maps.Copy(presets, bp)
	}

	// Merge user custom definitions on top (override by name).
	for name, def := range merged.Definitions {
		metrics[name] = pgwatchMetric{
			SQLs:            def.SQLs,
			Gauges:          def.Gauges,
			IsInstanceLevel: def.IsInstanceLevel,
			NodeStatus:      def.NodeStatus,
			Description:     def.Description,
		}
	}

	mf := pgwatchMetricsFile{Metrics: metrics, Presets: presets}

	// Use gopkg.in/yaml.v3 directly — sigs.k8s.io/yaml converts via JSON
	// which turns map[int]string keys into strings. pgwatch requires int keys.
	out, err := yamlv3.Marshal(mf)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func contentHash(data map[string]string) string {
	h := sha256.New()

	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(data[k]))
		h.Write([]byte{0})
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

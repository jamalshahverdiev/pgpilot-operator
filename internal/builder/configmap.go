package builder

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	yamlv3 "gopkg.in/yaml.v3"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
)

const (
	SourcesKey = "sources.yaml"
	MetricsKey = "metrics.yaml"
)

// BuildConfigMap creates the ConfigMap that pgwatch mounts as its
// configuration. It contains sources.yaml (connection + intervals) and, for
// a monitor that defines metrics of its own, metrics.yaml.
//
// The returned string is the SHA-256 hash of the combined data, used as
// the pgpilot.io/config-hash annotation on the Deployment pod template to
// trigger rollouts on config changes.
func BuildConfigMap(
	monitor *pgpilotv1.PgpilotMonitor,
	merged MergedMetrics,
) (*corev1.ConfigMap, string, error) {
	// Checked here, for every combination of preset and custom metrics: a
	// preset the collector cannot resolve makes it run and collect nothing,
	// which looks healthy from the outside.
	if err := ValidatePreset(merged.Preset); err != nil {
		return nil, "", err
	}

	sourcesYAML, err := renderSources(monitor, merged)
	if err != nil {
		return nil, "", fmt.Errorf("render sources.yaml: %w", err)
	}

	data := map[string]string{
		SourcesKey: sourcesYAML,
	}

	// metrics.yaml takes the place of the registry inside the image. A
	// monitor that only names a preset needs none, and is better off
	// without: it then runs on the definitions of the pgwatch release its
	// image holds, whichever that is, and its ConfigMap stays small.
	if NeedsMetricsFile(merged) {
		metricsYAML, err := renderMetrics(merged)
		if err != nil {
			return nil, "", fmt.Errorf("render metrics.yaml: %w", err)
		}
		data[MetricsKey] = metricsYAML
	}

	hash := contentHash(data)

	cm := &corev1.ConfigMap{
		APIVersion: "v1", Kind: "ConfigMap",
		Name:      ConfigMapName(monitor.Name, hash),
		Namespace: monitor.Namespace,
		Labels:    SystemLabels(monitor.Name),
		// What a name stands for does not change: the name is the hash of
		// the content. The API server holds the ConfigMap to that, so an
		// edit by hand cannot reach the pod that reads it, and the kubelet
		// need not watch it.
		Immutable: new(true),
		Data:      data,
	}

	return cm, hash, nil
}

// configNameHashLength is how much of the hash of a configuration its
// ConfigMap carries in its name: forty bits, which is plenty to tell the few
// configurations one monitor has at a time apart.
const configNameHashLength = 10

// ConfigMapName is the name of the ConfigMap that holds one rendered
// configuration of a monitor: pgpilot-<monitor>-config-<hash>.
//
// Each configuration has a ConfigMap of its own, and each pod template names
// the one it was made for. pgwatch reads its files again every two minutes,
// so a ConfigMap that changed in place reached the pod of the previous
// template as well: a pod that is kept because its successor cannot start,
// and that was then handed a configuration written for the successor, with
// a metrics file that was no longer there or certificates it never mounted.
func ConfigMapName(monitorName, hash string) string {
	return configMapPrefix(monitorName) + hash[:min(len(hash), configNameHashLength)]
}

// LegacyConfigMapName is what the one ConfigMap of a monitor was called
// before each configuration had its own.
func LegacyConfigMapName(monitorName string) string {
	return ResourceName(monitorName) + "-config"
}

func configMapPrefix(monitorName string) string {
	return LegacyConfigMapName(monitorName) + "-"
}

// IsConfigMapName reports whether a name is one a ConfigMap of the monitor
// has or had: the one of a configuration, or the one of earlier releases.
func IsConfigMapName(monitorName, name string) bool {
	if name == LegacyConfigMapName(monitorName) {
		return true
	}
	hash, found := strings.CutPrefix(name, configMapPrefix(monitorName))
	if !found || len(hash) != configNameHashLength {
		return false
	}
	for _, c := range hash {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
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
		CustomTags: sourceTags(monitor),
	}

	// pgwatch's source resolution logic sets md.Metrics = GetPresetMetrics(preset)
	// when PresetMetrics is set, fully REPLACING any custom_metrics. That means
	// "preset + custom_metrics" in the same source never works as addition.
	//
	// To support preset + custom together we expand the preset ourselves into
	// custom_metrics (preset's metric intervals + user's custom intervals) and
	// skip preset_metrics. When only preset is set, we use preset_metrics as-is
	// and pgwatch resolves it from the registry inside its image.
	switch {
	case merged.Preset != "" && len(merged.Intervals) > 0:
		expanded, err := expandPresetIntoIntervals(merged)
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

// NamespaceTag is the custom tag that carries the namespace of the monitor.
// pgwatch turns custom tags into labels of every series, and into fields of
// what it sends to a gRPC receiver.
const NamespaceTag = "namespace"

// sourceTags returns the custom tags of the source: the monitor's own, and
// its namespace. The name of the source, which becomes the dbname label, is
// the name of the monitor alone, and two monitors of one name in two
// namespaces are otherwise one database to whatever receives their
// measurements. The tag is the operator's: a namespace a monitor claims for
// itself in customTags does not replace the one it is in.
func sourceTags(monitor *pgpilotv1.PgpilotMonitor) map[string]string {
	tags := make(map[string]string, len(monitor.Spec.Database.CustomTags)+1)
	maps.Copy(tags, monitor.Spec.Database.CustomTags)
	tags[NamespaceTag] = monitor.Namespace
	return tags
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
	// The URL is assembled field by field instead of with a format string:
	// an IPv6 address has to be bracketed (given bare, "fd00::10:5432" reads
	// as a different address), and a database name may contain characters
	// such as '#', '?' or '%' that would otherwise cut the URL short and
	// drop the sslmode after it. For plain host and database names the
	// result is identical to the previous format.
	host := strings.TrimSuffix(strings.TrimPrefix(db.Host, "["), "]")
	params := url.Values{"sslmode": {string(sslmode)}}
	databaseTLSParams(db, params)
	u := url.URL{
		Scheme:   "postgresql",
		Host:     net.JoinHostPort(host, strconv.Itoa(int(port))),
		Path:     "/" + db.Database,
		RawQuery: params.Encode(),
	}
	return u.String()
}

// pgwatch metrics.yaml top-level structure.
// Uses yaml tags (not json) because renderMetrics uses gopkg.in/yaml.v3 directly.
// A metric is a pgwatchMetric for one of the monitor's own and a *yaml.Node
// for a built-in one, a preset is always a *yaml.Node.
type pgwatchMetricsFile struct {
	Metrics map[string]any          `yaml:"metrics,omitempty"`
	Presets map[string]*yamlv3.Node `yaml:"presets,omitempty"`
}

// pgwatchMetric is a metric definition of a monitor or a library, as pgwatch
// reads it. Built-in definitions do not pass through this struct.
type pgwatchMetric struct {
	SQLs            map[int]string `yaml:"sqls"`
	Gauges          []string       `yaml:"gauges,omitempty"`
	IsInstanceLevel bool           `yaml:"is_instance_level,omitempty"`
	NodeStatus      string         `yaml:"node_status,omitempty"`
	Description     string         `yaml:"description,omitempty"`
}

// pgwatchPreset is what the operator reads of a preset: what it collects,
// and how often. A preset is written out as the registry has it, not through
// this struct.
type pgwatchPreset struct {
	Metrics map[string]int `yaml:"metrics"`
}

// NeedsMetricsFile reports whether a monitor is given a metrics.yaml, and the
// collector the --metrics flag that points at it: when it has metric
// definitions of its own.
func NeedsMetricsFile(merged MergedMetrics) bool {
	return len(merged.Definitions) > 0
}

// renderMetrics builds the pgwatch metrics.yaml. When a preset is selected
// alongside custom metrics, we must ship pgwatch's entire built-in metric +
// preset registry because the --metrics flag REPLACES rather than merges with
// pgwatch's embedded defaults: the source lists the preset's metrics by name
// (see renderSources), and without the built-in definitions pgwatch would
// find none for them.
func renderMetrics(merged MergedMetrics) (string, error) {
	metrics := map[string]any{}
	var presets map[string]*yamlv3.Node

	// Start from the embedded pgwatch built-in registry when a preset is
	// requested: its metrics need their definitions.
	if merged.Preset != "" {
		registry, err := loadBuiltinRegistry()
		if err != nil {
			return "", err
		}
		for name, definition := range registry.metrics {
			metrics[name] = definition
		}
		presets = registry.presetNodes
	}

	// Merge user custom definitions on top (override by name).
	for name, def := range merged.Definitions {
		metrics[name] = pgwatchMetric(def)
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

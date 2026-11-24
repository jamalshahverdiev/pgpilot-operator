package builder

import (
	_ "embed"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"
)

const (
	// DefaultImageRepository is the pgwatch image a monitor runs when its
	// spec.image.repository is empty.
	DefaultImageRepository = "cybertecpostgresql/pgwatch"

	// PgwatchVersion is the pgwatch release this operator is built for. It is
	// the image tag of a monitor whose spec.image.tag is empty, and the
	// release the embedded metric registry below was taken from: the two
	// have to move together, because the registry is handed to the collector
	// in place of the one inside the image.
	//
	// The CRD deliberately has no default for spec.image.tag. A default
	// written by the API server is stored in every object and stays there
	// when the operator moves on, which is how monitors got stuck on an old
	// pgwatch before.
	//
	// To move to another release run `make update-pgwatch-registry
	// PGWATCH_VERSION=<x.y.z>` and set this constant to the same version;
	// the tests fail when the constant, pgwatchdata/VERSION and the registry
	// disagree.
	PgwatchVersion = "6.2.0"
)

// otherSourcePresets are presets pgwatch ships for source kinds other than
// PostgreSQL, by the source kind they belong to. Their metrics do have
// definitions, but the statements are commands of that product's admin
// console ("show stats", "show pool_nodes"), which a PostgreSQL server
// refuses. The operator only ever renders PostgreSQL sources.
var otherSourcePresets = map[string]string{
	"pgbouncer": "pgbouncer",
	"pgpool":    "pgpool",
}

// ErrInvalidPreset is wrapped by the errors returned for a spec.metrics.preset
// that cannot be collected from a PostgreSQL database. It is a mistake in the
// monitor, not a failure worth retrying.
var ErrInvalidPreset = errors.New("invalid pgwatch preset")

// builtinMetricsYAML is an unmodified copy of internal/metrics/metrics.yaml
// from the pgwatch release named by PgwatchVersion (pgwatchdata/VERSION
// records the same version). pgwatch is distributed under the BSD 3-Clause
// licence, reproduced in pgwatchdata/LICENSE.
//
// We need this because pgwatch's `--metrics <file>` flag REPLACES its embedded
// metrics registry rather than merging with it. So whenever we generate a
// metrics.yaml for a PgpilotMonitor that combines a built-in preset with user
// custom metrics, we must include every built-in metric and preset definition
// in our file — otherwise the preset resolves to an empty metric list.
//
//go:embed pgwatchdata/metrics.yaml
var builtinMetricsYAML []byte

// expandPresetIntoIntervals returns the merged metric-name → interval map:
// all metrics from the named built-in preset, with user-provided intervals
// taking precedence on name collisions. Returns an error if the preset is
// unknown.
func expandPresetIntoIntervals(presetName string, userIntervals map[string]int) (map[string]int, error) {
	_, presets, err := loadBuiltinMetrics()
	if err != nil {
		return nil, err
	}
	preset, ok := presets[presetName]
	if !ok {
		return nil, fmt.Errorf("%w: unknown preset %q", ErrInvalidPreset, presetName)
	}

	out := make(map[string]int, len(preset.Metrics)+len(userIntervals))
	maps.Copy(out, preset.Metrics)
	maps.Copy(out, userIntervals)
	return out, nil
}

// ValidatePreset reports whether the collector can gather the named built-in
// preset from a PostgreSQL database. An empty name is valid: it means the
// monitor uses no preset.
//
// Three kinds of names are refused. One that is not in the registry at all
// would leave the collector running with nothing to collect, since the
// registry handed to it is the only one it knows. The second is a preset that
// is in the registry but none of whose metrics has a definition there:
// pgwatch ships such presets ("patroni", "postgres-exporter-basic") for its
// Prometheus-scraping source kinds, where the entries name metric families of
// another exporter. The third are the presets for connection poolers
// (otherSourcePresets). The operator only renders PostgreSQL sources, for
// which all of these collect nothing.
func ValidatePreset(name string) error {
	if name == "" {
		return nil
	}
	metrics, presets, err := loadBuiltinMetrics()
	if err != nil {
		return err
	}
	preset, ok := presets[name]
	if !ok {
		return fmt.Errorf("%w: %q is not a preset of pgwatch %s (available: %s)",
			ErrInvalidPreset, name, PgwatchVersion, strings.Join(usablePresets(metrics, presets), ", "))
	}
	if kind, other := otherSourcePresets[name]; other {
		return fmt.Errorf("%w: %q is a pgwatch preset for %s sources, not for a PostgreSQL database "+
			"(available: %s)", ErrInvalidPreset, name, kind, strings.Join(usablePresets(metrics, presets), ", "))
	}
	if !hasDefinedMetric(preset, metrics) {
		return fmt.Errorf("%w: %q is a pgwatch preset for Prometheus-type sources and has no SQL metrics, "+
			"so it cannot be used to monitor a PostgreSQL database (available: %s)",
			ErrInvalidPreset, name, strings.Join(usablePresets(metrics, presets), ", "))
	}
	return nil
}

// Presets returns the names of the built-in presets a monitor can use, sorted.
func Presets() ([]string, error) {
	metrics, presets, err := loadBuiltinMetrics()
	if err != nil {
		return nil, err
	}
	return usablePresets(metrics, presets), nil
}

func usablePresets(metrics map[string]pgwatchMetric, presets map[string]pgwatchPreset) []string {
	names := make([]string, 0, len(presets))
	for name, preset := range presets {
		if _, other := otherSourcePresets[name]; other || !hasDefinedMetric(preset, metrics) {
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func hasDefinedMetric(preset pgwatchPreset, metrics map[string]pgwatchMetric) bool {
	for name := range preset.Metrics {
		if _, ok := metrics[name]; ok {
			return true
		}
	}
	return false
}

// loadBuiltinMetrics parses the embedded pgwatch metrics.yaml into the same
// shape we use when rendering custom metrics, so we can merge the two.
func loadBuiltinMetrics() (map[string]pgwatchMetric, map[string]pgwatchPreset, error) {
	var parsed struct {
		Metrics map[string]pgwatchMetric `yaml:"metrics"`
		Presets map[string]pgwatchPreset `yaml:"presets"`
	}
	if err := yamlv3.Unmarshal(builtinMetricsYAML, &parsed); err != nil {
		return nil, nil, fmt.Errorf("parse embedded pgwatch metrics.yaml: %w", err)
	}
	if parsed.Metrics == nil {
		parsed.Metrics = map[string]pgwatchMetric{}
	}
	if parsed.Presets == nil {
		parsed.Presets = map[string]pgwatchPreset{}
	}
	return parsed.Metrics, parsed.Presets, nil
}

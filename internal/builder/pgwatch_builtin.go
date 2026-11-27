package builder

import (
	_ "embed"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	yamlv3 "gopkg.in/yaml.v3"
)

const (
	// DefaultImageRepository is the pgwatch image a monitor runs when its
	// spec.image.repository is empty.
	DefaultImageRepository = "cybertecpostgresql/pgwatch"

	// PgwatchVersion is the pgwatch release this operator is built for. It is
	// the image tag of a monitor whose spec.image.tag is empty, and the
	// release the embedded metric registry below was taken from: the two
	// have to move together, because a monitor with metrics of its own is
	// handed that registry in place of the one inside the image, and the
	// names of the presets are checked against it for every monitor.
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
// It is needed for two things. The names of the presets are checked against
// it. And pgwatch's `--metrics <file>` flag REPLACES the registry inside the
// image rather than adding to it: a monitor that defines metrics of its own
// gets a metrics.yaml, and when it uses a preset as well, that file has to
// hold every built-in metric, or the metrics of the preset have no
// definition.
// A monitor that only uses a preset gets no metrics.yaml and runs on the
// registry of its image.
//
//go:embed pgwatchdata/metrics.yaml
var builtinMetricsYAML []byte

// builtinRegistry is the embedded registry, parsed.
type builtinRegistry struct {
	// metrics and presetNodes hold every metric and preset as the file
	// writes it, so that it is handed on with nothing lost: a key pgwatch
	// gives a meaning to in a later release is not for the operator to drop.
	metrics     map[string]*yamlv3.Node
	presetNodes map[string]*yamlv3.Node
	// presets says what each preset collects, and how often.
	presets map[string]pgwatchPreset
}

// loadBuiltinRegistry parses the embedded registry, once: it is a quarter of
// a megabyte, and it is asked for several times in every reconcile. What it
// returns is shared and must not be changed.
var loadBuiltinRegistry = sync.OnceValues(func() (*builtinRegistry, error) {
	return parseRegistry(builtinMetricsYAML)
})

func parseRegistry(raw []byte) (*builtinRegistry, error) {
	// Nodes by value: the decoder fills a yaml.Node it is handed, and takes
	// a pointer to one for a struct to decode into field by field.
	var parsed struct {
		Metrics map[string]yamlv3.Node `yaml:"metrics"`
		Presets map[string]yamlv3.Node `yaml:"presets"`
	}
	if err := yamlv3.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("parse embedded pgwatch metrics.yaml: %w", err)
	}
	registry := &builtinRegistry{
		metrics:     make(map[string]*yamlv3.Node, len(parsed.Metrics)),
		presetNodes: make(map[string]*yamlv3.Node, len(parsed.Presets)),
		presets:     make(map[string]pgwatchPreset, len(parsed.Presets)),
	}
	for name, node := range parsed.Metrics {
		registry.metrics[name] = &node
	}
	for name, node := range parsed.Presets {
		var preset pgwatchPreset
		if err := node.Decode(&preset); err != nil {
			return nil, fmt.Errorf("parse embedded pgwatch metrics.yaml: preset %q: %w", name, err)
		}
		registry.presets[name], registry.presetNodes[name] = preset, &node
	}
	return registry, nil
}

// expandPresetIntoIntervals returns what a source collects that uses a
// preset next to metrics of its own: every metric of the preset at the
// preset's interval, and the monitor's metrics at theirs.
//
// A metric of the monitor that carries the name of one in the preset
// replaces its definition. Its interval replaces the preset's only when the
// definition names one: a definition that changes the statement of
// table_bloat_approx_summary_sql and says nothing about the interval is not
// a request to run it every minute instead of every two hours.
func expandPresetIntoIntervals(merged MergedMetrics) (map[string]int, error) {
	registry, err := loadBuiltinRegistry()
	if err != nil {
		return nil, err
	}
	preset, ok := registry.presets[merged.Preset]
	if !ok {
		return nil, fmt.Errorf("%w: unknown preset %q", ErrInvalidPreset, merged.Preset)
	}

	out := make(map[string]int, len(preset.Metrics)+len(merged.Intervals))
	maps.Copy(out, preset.Metrics)
	for name, interval := range merged.Intervals {
		if _, inPreset := preset.Metrics[name]; inPreset && merged.IntervalUnset[name] {
			continue
		}
		out[name] = interval
	}
	return out, nil
}

// ValidatePreset reports whether the collector can gather the named built-in
// preset from a PostgreSQL database. An empty name is valid: it means the
// monitor uses no preset.
//
// The name is looked up in the registry of the pgwatch release the operator
// is built for, also for a monitor whose spec.image names another release:
// the operator cannot look into an image, and a preset it knows nothing
// about is more likely a typo than a preset only that image has.
//
// Three kinds of names are refused. One that is not in the registry at all
// would leave the collector running with nothing to collect. The second is a preset that
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
	registry, err := loadBuiltinRegistry()
	if err != nil {
		return err
	}
	preset, ok := registry.presets[name]
	if !ok {
		return fmt.Errorf("%w: %q is not a preset of pgwatch %s (available: %s)",
			ErrInvalidPreset, name, PgwatchVersion, strings.Join(registry.usablePresets(), ", "))
	}
	if kind, other := otherSourcePresets[name]; other {
		return fmt.Errorf("%w: %q is a pgwatch preset for %s sources, not for a PostgreSQL database "+
			"(available: %s)", ErrInvalidPreset, name, kind, strings.Join(registry.usablePresets(), ", "))
	}
	if !hasDefinedMetric(preset, registry.metrics) {
		return fmt.Errorf("%w: %q is a pgwatch preset for Prometheus-type sources and has no SQL metrics, "+
			"so it cannot be used to monitor a PostgreSQL database (available: %s)",
			ErrInvalidPreset, name, strings.Join(registry.usablePresets(), ", "))
	}
	return nil
}

// Presets returns the names of the built-in presets a monitor can use, sorted.
func Presets() ([]string, error) {
	registry, err := loadBuiltinRegistry()
	if err != nil {
		return nil, err
	}
	return registry.usablePresets(), nil
}

func (r *builtinRegistry) usablePresets() []string {
	names := make([]string, 0, len(r.presets))
	for name, preset := range r.presets {
		if _, other := otherSourcePresets[name]; other || !hasDefinedMetric(preset, r.metrics) {
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func hasDefinedMetric(preset pgwatchPreset, metrics map[string]*yamlv3.Node) bool {
	for name := range preset.Metrics {
		if _, ok := metrics[name]; ok {
			return true
		}
	}
	return false
}

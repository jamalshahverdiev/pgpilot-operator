package builder

import (
	_ "embed"
	"fmt"
	"maps"

	yamlv3 "gopkg.in/yaml.v3"
)

// builtinMetricsYAML is a copy of pgwatch's own internal/metrics/metrics.yaml
// for the pgwatch version we ship against (see CHANGELOG / Chart.yaml.appVersion).
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
		return nil, fmt.Errorf("unknown pgwatch preset %q", presetName)
	}

	out := make(map[string]int, len(preset.Metrics)+len(userIntervals))
	maps.Copy(out, preset.Metrics)
	maps.Copy(out, userIntervals)
	return out, nil
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

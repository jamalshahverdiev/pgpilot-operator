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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
)

// The builder tests show that sources.yaml names the namespace. That pgwatch
// turns it into a label of every series of the source, and that a namespace
// the monitor claims in customTags does not arrive, is only shown here.
func TestSeriesCarryTheNamespaceOfTheMonitor(t *testing.T) {
	t.Parallel()
	monitor := newMonitor("ns-label", pgpilotv1.MetricsSpec{
		Preset: "basic",
		Custom: []pgpilotv1.MetricDefinition{{
			Name:     "queue_depth",
			Interval: &metav1.Duration{Duration: 5 * time.Second},
			SQLs:     map[string]string{"14": "SELECT (extract(epoch from now()) * 1e9)::int8 AS epoch_ns, 42::int8 AS depth"},
			Gauges:   []string{"*"},
		}},
	})
	// Not the value of the env tag, so that the two cannot be mixed up.
	monitor.Namespace = "team-a"
	monitor.Spec.Database.CustomTags["namespace"] = "somebody-elses"
	c := startCollector(t, monitor, nil, defaultSecrets())

	want := map[string]string{"dbname": monitor.Name, "namespace": "team-a", "env": "collector-test"}
	got := c.eventually("series with the namespace of the monitor", func(got samples) error {
		for _, name := range []string{"pgwatch_instance_up", "pgwatch_db_size_size_b", "pgwatch_queue_depth_depth"} {
			if len(got.find(name, want)) == 0 {
				return errNoSample(name)
			}
		}
		return nil
	})
	for _, s := range got {
		if _, ofTheSource := s.labels["dbname"]; !ofTheSource {
			continue // pgwatch's own counters (pgwatch_exporter_*) carry no source
		}
		if s.labels["namespace"] != "team-a" {
			t.Errorf("%s%v does not carry the namespace of the monitor", s.name, s.labels)
		}
	}
	c.expectCleanLog()
}

type errNoSample string

func (e errNoSample) Error() string { return "no sample of " + string(e) }

// A column tag_namespace of a metric wins over the custom tag: pgwatch
// writes the tag columns over the custom tags (WritePromMetrics). The series
// of such a metric carry the column's value as "namespace", not the
// monitor's namespace. Behind the generated ServiceMonitor that label is
// renamed to exported_namespace and then dropped by the labeldrop rule, so
// the series of the metric are left with nothing to tell them apart.
//
// This test states what pgwatch does today; it is here to show the case, and
// passes.
func TestATagNamespaceColumnTakesThePlaceOfTheMonitorsNamespace(t *testing.T) {
	t.Parallel()
	monitor := newMonitor("ns-column", pgpilotv1.MetricsSpec{
		Custom: []pgpilotv1.MetricDefinition{{
			Name:     "schema_tables",
			Interval: &metav1.Duration{Duration: 5 * time.Second},
			SQLs: map[string]string{"14": "SELECT (extract(epoch from now()) * 1e9)::int8 AS epoch_ns, " +
				"n.nspname::text AS tag_namespace, count(*)::int8 AS tables " +
				"FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace " +
				"WHERE n.nspname IN ('pg_catalog', 'information_schema') GROUP BY n.nspname"},
			Gauges: []string{"*"},
		}},
	})
	monitor.Namespace = "team-a"
	c := startCollector(t, monitor, nil, defaultSecrets())

	got := c.eventually("one series per schema", func(got samples) error {
		if len(got.find("pgwatch_schema_tables_tables", map[string]string{"dbname": monitor.Name})) != 2 {
			return errNoSample("pgwatch_schema_tables_tables, two of them")
		}
		return nil
	})
	seen := map[string]bool{}
	for _, s := range got.find("pgwatch_schema_tables_tables", nil) {
		seen[s.labels["namespace"]] = true
		// What is left of the series once exported_namespace is dropped.
		delete(s.labels, "namespace")
		t.Logf("without its namespace label: pgwatch_schema_tables_tables%v %v", s.labels, s.value)
	}
	if !seen["pg_catalog"] || !seen["information_schema"] || seen["team-a"] {
		t.Errorf("namespace labels of the metric: %v; pgwatch no longer lets the tag column win", seen)
	}
}

// The gRPC sink: the envelope carries the custom tags as a map, and the
// namespace is one of them.
func TestGRPCEnvelopeNamesTheNamespace(t *testing.T) {
	t.Parallel()
	sink := startReceiver(t)
	monitor := grpcOnly("grpc-namespace", sink.endpoint)
	monitor.Namespace = "team-a"
	c := startCollector(t, monitor, nil, defaultSecrets())

	// A map entry of the envelope: field 1 the key, field 2 the value.
	entry := []byte("\x0a\x09namespace\x12\x06team-a")
	delivered := func() bool {
		for _, message := range sink.received("/UpdateMeasurements") {
			if bytes.Contains(message, []byte("grpc-namespace")) && bytes.Contains(message, entry) {
				return true
			}
		}
		return false
	}
	for deadline := time.Now().Add(firstSamples); !delivered(); time.Sleep(time.Second) {
		if !c.running() {
			t.Fatalf("the collector exited")
		}
		if time.Now().After(deadline) {
			t.Fatalf("no measurement with the custom tag namespace=team-a arrived within %s; the receiver saw %v",
				firstSamples, sink.methods())
		}
	}
}

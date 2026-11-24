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

package v1

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestObjectMetadataOverrides_LabelMap(t *testing.T) {
	t.Run("nil labels give a nil map", func(t *testing.T) {
		if got := (ObjectMetadataOverrides{}).LabelMap(); got != nil {
			t.Errorf("LabelMap() = %v, want nil", got)
		}
	})

	t.Run("empty labels give an empty map", func(t *testing.T) {
		got := ObjectMetadataOverrides{Labels: map[string]LabelValue{}}.LabelMap()
		if got == nil || len(got) != 0 {
			t.Errorf("LabelMap() = %#v, want an empty, non-nil map", got)
		}
	})

	t.Run("values are converted", func(t *testing.T) {
		in := ObjectMetadataOverrides{Labels: map[string]LabelValue{
			"team":                    "data",
			"example.com/cost-center": "cc_1.2-x",
			"empty":                   "",
		}}
		want := map[string]string{
			"team":                    "data",
			"example.com/cost-center": "cc_1.2-x",
			"empty":                   "",
		}
		if got := in.LabelMap(); !reflect.DeepEqual(got, want) {
			t.Errorf("LabelMap() = %v, want %v", got, want)
		}
	})

	t.Run("the result is a copy", func(t *testing.T) {
		in := ObjectMetadataOverrides{Labels: map[string]LabelValue{"team": "data"}}
		out := in.LabelMap()
		out["team"] = "changed"
		out["added"] = "x"
		if in.Labels["team"] != "data" || len(in.Labels) != 1 {
			t.Errorf("modifying the result changed the spec: %v", in.Labels)
		}
	})
}

// The typed label value must not change how the field looks on the wire.
func TestObjectMetadataOverrides_JSONShape(t *testing.T) {
	in := ObjectMetadataOverrides{
		Labels:      map[string]LabelValue{"team": "data"},
		Annotations: map[string]string{"prometheus.io/scrape": "true"},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"labels":{"team":"data"},"annotations":{"prometheus.io/scrape":"true"}}`
	if string(raw) != want {
		t.Errorf("JSON = %s, want %s", raw, want)
	}

	var out ObjectMetadataOverrides
	if err := json.Unmarshal([]byte(want), &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Errorf("round trip = %#v, want %#v", out, in)
	}
}

// status.ready must be written even when false; kubectl shows an empty column
// for a missing field.
func TestPgpilotMonitorStatus_ReadyIsAlwaysSerialized(t *testing.T) {
	raw, err := json.Marshal(PgpilotMonitorStatus{})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	ready, ok := fields["ready"]
	if !ok {
		t.Fatalf("status JSON has no ready field: %s", raw)
	}
	if ready != false {
		t.Errorf("ready = %v, want false", ready)
	}
}

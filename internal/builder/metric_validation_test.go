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

package builder

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
)

func usableMetric(name string) pgpilotv1.MetricDefinition {
	return pgpilotv1.MetricDefinition{Name: name, SQLs: map[string]string{"14": "SELECT 1"}}
}

func TestValidateMetricDefinitions_Accepts(t *testing.T) {
	second := metav1.Duration{Duration: time.Second}
	full := pgpilotv1.MetricDefinition{
		Name:        "_orders_queue_depth_2",
		Description: "anything at all",
		Interval:    &second,
		SQLs:        map[string]string{"0": "SELECT 1", "9": "SELECT 2", "14": "SELECT 3", "999": "SELECT 4"},
		Gauges:      []string{"*"},
		MasterOnly:  true,
		// Ignored: whatever is stored there is no reason to refuse the
		// metric.
		StatementTimeoutSeconds: -1, //nolint:staticcheck // SA1019: the deprecated field is the point
	}
	standby := usableMetric("lag")
	standby.StandbyOnly = true
	longest := usableMetric(strings.Repeat("m", 63))

	for name, defs := range map[string][]pgpilotv1.MetricDefinition{
		"nothing":                  nil,
		"one metric":               {usableMetric("orders")},
		"every field in use":       {full},
		"standby only":             {standby},
		"a name of 63 characters":  {longest},
		"several metrics":          {usableMetric("a"), usableMetric("b"), full},
		"an empty list":            {},
		"no interval: the default": {usableMetric("no_interval")},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateMetricDefinitions(defs); err != nil {
				t.Errorf("got %v", err)
			}
		})
	}
}

func TestValidateMetricDefinitions_Refuses(t *testing.T) {
	with := func(change func(*pgpilotv1.MetricDefinition)) []pgpilotv1.MetricDefinition {
		md := usableMetric("probe")
		change(&md)
		return []pgpilotv1.MetricDefinition{md}
	}
	sqls := func(m map[string]string) []pgpilotv1.MetricDefinition {
		return with(func(md *pgpilotv1.MetricDefinition) { md.SQLs = m })
	}
	named := func(name string) []pgpilotv1.MetricDefinition {
		return with(func(md *pgpilotv1.MetricDefinition) { md.Name = name })
	}
	const notAVersion = "is not a PostgreSQL major version"
	const badName = "the name must match ^[a-z_][a-z0-9_]*$ and have at most 63 characters"

	cases := map[string]struct {
		defs []pgpilotv1.MetricDefinition
		want string // the whole message
	}{
		// What MergeMetrics does with a key that is no number ("*", "14.2",
		// "v14", " 14", "14\n", "", "latest"): it drops it, and a metric
		// whose only key it was is written out without any statement. The
		// others it reads as numbers: "+14" and "014" were collected as 14,
		// "-1", "1000" and "170000" never match a server.
		"a wildcard key": {
			sqls(map[string]string{"*": "SELECT 1"}),
			`metric "probe": sqls key "*" ` + notAVersion + ` written as a plain integer, such as "14" ("0" matches every version)`,
		},
		"a minor version":           {sqls(map[string]string{"14.2": "SELECT 1"}), `metric "probe": sqls key "14.2" ` + notAVersion},
		"a v prefix":                {sqls(map[string]string{"v14": "SELECT 1"}), `sqls key "v14" ` + notAVersion},
		"a plus sign":               {sqls(map[string]string{"+14": "SELECT 1"}), `sqls key "+14" ` + notAVersion},
		"a leading zero":            {sqls(map[string]string{"014": "SELECT 1"}), `sqls key "014" ` + notAVersion},
		"a negative number":         {sqls(map[string]string{"-1": "SELECT 1"}), `sqls key "-1" ` + notAVersion},
		"a leading space":           {sqls(map[string]string{" 14": "SELECT 1"}), `sqls key " 14" ` + notAVersion},
		"a trailing newline":        {sqls(map[string]string{"14\n": "SELECT 1"}), `sqls key "14\n" ` + notAVersion},
		"an empty key":              {sqls(map[string]string{"": "SELECT 1"}), `sqls key "" ` + notAVersion},
		"four digits":               {sqls(map[string]string{"1000": "SELECT 1"}), `sqls key "1000" ` + notAVersion},
		"a server_version_num":      {sqls(map[string]string{"170000": "SELECT 1"}), `sqls key "170000" ` + notAVersion},
		"a good key next to a bad":  {sqls(map[string]string{"14": "SELECT 1", "latest": "SELECT 2"}), `sqls key "latest" ` + notAVersion},
		"an empty statement":        {sqls(map[string]string{"14": ""}), `metric "probe": the statement under sqls key "14" is empty`},
		"no statement at all":       {sqls(map[string]string{}), `metric "probe": sqls holds no statement`},
		"sqls not set":              {sqls(nil), `metric "probe": sqls holds no statement`},
		"both roles":                {with(func(md *pgpilotv1.MetricDefinition) { md.MasterOnly, md.StandbyOnly = true, true }), `metric "probe": masterOnly and standbyOnly are both set`},
		"an interval below 1s":      {with(func(md *pgpilotv1.MetricDefinition) { md.Interval = &metav1.Duration{Duration: 999 * time.Millisecond} }), `metric "probe": interval 999ms is below the minimum of 1s`},
		"an interval of zero":       {with(func(md *pgpilotv1.MetricDefinition) { md.Interval = &metav1.Duration{} }), `metric "probe": interval 0s is below the minimum of 1s`},
		"a negative interval":       {with(func(md *pgpilotv1.MetricDefinition) { md.Interval = &metav1.Duration{Duration: -time.Minute} }), `interval -1m0s is below the minimum of 1s`},
		"an upper-case name":        {named("OrdersQueue"), `metric "OrdersQueue": ` + badName},
		"a dash in the name":        {named("orders-queue"), `metric "orders-queue": ` + badName},
		"a name with leading digit": {named("1orders"), `metric "1orders": ` + badName},
		"an empty name":             {named(""), `metric "": ` + badName},
		"a name of 64 characters":   {named(strings.Repeat("m", 64)), badName},
		"the same name twice": {
			[]pgpilotv1.MetricDefinition{usableMetric("twice"), usableMetric("once"), usableMetric("twice")},
			`metric "twice" is defined more than once`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidateMetricDefinitions(tc.defs)
			if err == nil {
				t.Fatal("accepted")
			}
			if !errors.Is(err, ErrInvalidMetric) {
				t.Errorf("errors.Is(err, ErrInvalidMetric) is false for %T", err)
			}
			var invalid *InvalidMetricsError
			if !errors.As(err, &invalid) || len(invalid.Problems) != 1 {
				t.Fatalf("got %v, want exactly one problem", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got  %q\nwant %q", err.Error(), tc.want)
			}
		})
	}
}

// Everything that is wrong is found in one go, in an order that does not
// change from one reconcile to the next: the message goes into a status
// condition, and a status that changes is written and announced again.
func TestValidateMetricDefinitions_EveryProblemInAFixedOrder(t *testing.T) {
	first := pgpilotv1.MetricDefinition{
		Name:        "Bad-Name",
		SQLs:        map[string]string{"*": "SELECT 1", "14": "", "9": "SELECT 1", "x": ""},
		MasterOnly:  true,
		StandbyOnly: true,
		Interval:    &metav1.Duration{Duration: time.Millisecond},
	}
	defs := []pgpilotv1.MetricDefinition{first, usableMetric("fine"), {Name: "empty"}, usableMetric("fine"), usableMetric("fine")}
	want := []string{
		`metric "Bad-Name": the name must match ^[a-z_][a-z0-9_]*$ and have at most 63 characters`,
		`metric "Bad-Name": sqls key "*" is not a PostgreSQL major version written as a plain integer, such as "14" ("0" matches every version)`,
		`metric "Bad-Name": the statement under sqls key "14" is empty`,
		`metric "Bad-Name": sqls key "x" is not a PostgreSQL major version written as a plain integer, such as "14" ("0" matches every version)`,
		`metric "Bad-Name": the statement under sqls key "x" is empty`,
		`metric "Bad-Name": masterOnly and standbyOnly are both set; a metric is collected on one of the two, or on both by setting neither`,
		`metric "Bad-Name": interval 1ms is below the minimum of 1s`,
		`metric "empty": sqls holds no statement`,
		// Said once, however often the name comes back.
		`metric "fine" is defined more than once`,
	}
	for range 20 {
		var invalid *InvalidMetricsError
		if err := ValidateMetricDefinitions(defs); !errors.As(err, &invalid) {
			t.Fatalf("got %v", err)
		}
		if got := strings.Join(invalid.Problems, "\n"); got != strings.Join(want, "\n") {
			t.Fatalf("got\n%s\nwant\n%s", got, strings.Join(want, "\n"))
		}
	}
}

// The message ends up in a status condition: a library with hundreds of
// broken definitions must not turn into a status of that size.
func TestInvalidMetricsError_NamesTheFirstFewAndCountsTheRest(t *testing.T) {
	problems := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("problem %d", i+1)
		}
		return out
	}
	for n, want := range map[int]string{
		1: "problem 1",
		5: "problem 1; problem 2; problem 3; problem 4; problem 5",
		6: "problem 1; problem 2; problem 3; problem 4; problem 5; and 1 more",
		9: "problem 1; problem 2; problem 3; problem 4; problem 5; and 4 more",
	} {
		if got := (&InvalidMetricsError{Problems: problems(n)}).Error(); got != want {
			t.Errorf("%d problems: got %q, want %q", n, got, want)
		}
	}
	if errors.Is(&InvalidMetricsError{}, ErrInvalidPreset) {
		t.Error("an invalid metric is taken for an invalid preset")
	}
}

// Keys that are made of digits and are no major version all the same.
func TestValidateMetricDefinitions_RefusesKeysThatOnlyLookLikeAVersion(t *testing.T) {
	for _, key := range []string{"00", "000", "1\u0664", "\u0661\u0664"} {
		md := pgpilotv1.MetricDefinition{Name: "probe", SQLs: map[string]string{key: "SELECT 1"}}
		err := ValidateMetricDefinitions([]pgpilotv1.MetricDefinition{md})
		if !errors.Is(err, ErrInvalidMetric) {
			t.Errorf("sqls key %q: got %v, want it refused", key, err)
		}
	}
}

// The CRD asks for a statement that is not empty and nothing else. A check
// that is stricter here would stop a monitor for a definition the API server
// accepts as it is.
func TestValidateMetricDefinitions_AcceptsAStatementOfWhiteSpace(t *testing.T) {
	md := pgpilotv1.MetricDefinition{Name: "probe", SQLs: map[string]string{"14": " \n\t"}}
	if err := ValidateMetricDefinitions([]pgpilotv1.MetricDefinition{md}); err != nil {
		t.Errorf("got %v; the API server accepts this definition", err)
	}
}

// The cases of TestValidateMetricDefinitions_Refuses compare the message,
// which holds the pattern as text. These are about what the pattern accepts:
// an upper-case letter in the tail only or at the head only, a second line,
// a letter that is not ASCII.
func TestValidateMetricDefinitions_RefusesNamesThatAreNoLowerCaseIdentifiers(t *testing.T) {
	for _, name := range []string{"ordersQueue", "Orders", "orders\n", "orders\nqueue", "ord\u00e9rs"} {
		md := pgpilotv1.MetricDefinition{Name: name, SQLs: map[string]string{"14": "SELECT 1"}}
		err := ValidateMetricDefinitions([]pgpilotv1.MetricDefinition{md})
		if !errors.Is(err, ErrInvalidMetric) {
			t.Errorf("name %q: got %v, want it refused", name, err)
		}
	}
}

// Nothing limited the length of a name or of a sqls key before the rules,
// and the text ends up in a status condition, which holds 32768 bytes and is
// refused as a whole beyond that.
func TestValidateMetricDefinitions_BoundsWhatItQuotes(t *testing.T) {
	message := func(md pgpilotv1.MetricDefinition) string {
		t.Helper()
		err := ValidateMetricDefinitions([]pgpilotv1.MetricDefinition{md})
		if err == nil {
			t.Fatal("accepted")
		}
		return err.Error()
	}

	t.Run("a key of 40000 characters", func(t *testing.T) {
		got := message(pgpilotv1.MetricDefinition{Name: "probe", SQLs: map[string]string{
			"14": "SELECT 1", strings.Repeat("k", 40000): "",
		}})
		shown := `"` + strings.Repeat("k", 63) + `"... (40000 characters)`
		want := `metric "probe": sqls key ` + shown + ` is not a PostgreSQL major version written as a plain integer, ` +
			`such as "14" ("0" matches every version); metric "probe": the statement under sqls key ` + shown + ` is empty`
		if got != want {
			t.Errorf("got  %q\nwant %q", got, want)
		}
	})

	t.Run("a name of 5000 characters, also where it is named twice", func(t *testing.T) {
		long := pgpilotv1.MetricDefinition{Name: strings.Repeat("m", 5000), SQLs: map[string]string{"14": "SELECT 1"}}
		err := ValidateMetricDefinitions([]pgpilotv1.MetricDefinition{long, long})
		if err == nil {
			t.Fatal("accepted")
		}
		shown := `metric "` + strings.Repeat("m", 63) + `"... (5000 characters)`
		for _, want := range []string{shown + ": the name must match", shown + " is defined more than once"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("got %q, want it to hold %q", err.Error(), want)
			}
		}
		if len(err.Error()) > 1024 {
			t.Errorf("%d bytes", len(err.Error()))
		}
	})

	t.Run("63 characters are shown whole", func(t *testing.T) {
		key := strings.Repeat("k", 63)
		got := message(pgpilotv1.MetricDefinition{Name: "probe", SQLs: map[string]string{key: "SELECT 1"}})
		if !strings.Contains(got, `sqls key "`+key+`" is not`) {
			t.Errorf("got %q", got)
		}
	})

	t.Run("characters, not bytes", func(t *testing.T) {
		got := message(pgpilotv1.MetricDefinition{Name: "probe", SQLs: map[string]string{
			strings.Repeat("\u00e9", 64): "SELECT 1",
		}})
		if !strings.Contains(got, `sqls key "`+strings.Repeat("\u00e9", 63)+`"... (64 characters) is not`) || !utf8.ValidString(got) {
			t.Errorf("got %q", got)
		}
	})

	// Characters that are written as escapes take up to ten bytes each.
	t.Run("the text as a whole", func(t *testing.T) {
		defs := make([]pgpilotv1.MetricDefinition, 0, 6)
		for i := range 5 {
			defs = append(defs, pgpilotv1.MetricDefinition{
				Name: strings.Repeat(string(rune(0x10FFF0+i)), 63),
				SQLs: map[string]string{strings.Repeat("\U0010FFFF", 63): "SELECT 1"},
			})
		}
		defs = append(defs, pgpilotv1.MetricDefinition{Name: "one_more"})
		got := ValidateMetricDefinitions(defs).Error()
		const ending = "...; and 6 more"
		if len(got) > maxProblemsText+len(ending) || !strings.HasSuffix(got, ending) || !utf8.ValidString(got) {
			t.Errorf("%d bytes, ending in %q", len(got), got[max(0, len(got)-40):])
		}
		if !strings.HasPrefix(got, `metric "\U0010fff0`) {
			t.Errorf("begins with %q", got[:40])
		}
	})

	// The cut must not split a character that takes several bytes.
	t.Run("a cut inside a character", func(t *testing.T) {
		for pad := range 4 {
			e := &InvalidMetricsError{Problems: []string{strings.Repeat("x", pad) + strings.Repeat("\u20ac", 1000)}}
			got := e.Error()
			if !utf8.ValidString(got) || !strings.HasSuffix(got, "\u20ac...") ||
				len(got) > maxProblemsText+3 || len(got) < maxProblemsText {
				t.Errorf("padding %d: %d bytes, ending in %q", pad, len(got), got[len(got)-8:])
			}
		}
	})

	t.Run("one byte beyond the bound is cut at the bound", func(t *testing.T) {
		e := &InvalidMetricsError{Problems: []string{strings.Repeat("x", maxProblemsText+1)}}
		if got := e.Error(); got != strings.Repeat("x", maxProblemsText)+"..." {
			t.Errorf("%d bytes, ending in %q", len(got), got[len(got)-8:])
		}
	})

	t.Run("a text of exactly the bound is left alone", func(t *testing.T) {
		e := &InvalidMetricsError{Problems: []string{strings.Repeat("x", maxProblemsText)}}
		if got := e.Error(); got != strings.Repeat("x", maxProblemsText) {
			t.Errorf("%d bytes, ending in %q", len(got), got[len(got)-8:])
		}
	})
}

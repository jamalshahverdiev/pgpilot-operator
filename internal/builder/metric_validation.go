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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
)

// ErrInvalidMetric is matched by every error ValidateMetricDefinitions
// returns.
var ErrInvalidMetric = errors.New("invalid metric definition")

var (
	metricNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	// sqlsKeyPattern is a PostgreSQL major version as pgwatch reads it: a
	// plain integer, "0" for every version.
	sqlsKeyPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,2})$`)
)

const (
	maxMetricNameLength = 63
	minMetricInterval   = time.Second
	// shownMetricProblems bounds how many problems an error spells out: it
	// ends up in a status condition.
	shownMetricProblems = 5
	// shownCharacters bounds how much of a metric name or of a sqls key is
	// quoted. Nothing limited their length before the rules, and a
	// condition message that is too long is refused as a whole.
	shownCharacters = 63
	// maxProblemsText bounds the text of an error, whatever it is made of.
	maxProblemsText = 2048
)

// InvalidMetricsError lists what is wrong with a set of metric definitions.
type InvalidMetricsError struct {
	// Problems holds one sentence per problem, each naming its metric.
	Problems []string
}

func (e *InvalidMetricsError) Error() string {
	shown := e.Problems
	if len(shown) > shownMetricProblems {
		shown = shown[:shownMetricProblems]
	}
	text := strings.Join(shown, "; ")
	if len(text) > maxProblemsText {
		cut := maxProblemsText
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut] + "..."
	}
	if more := len(e.Problems) - len(shown); more > 0 {
		text += fmt.Sprintf("; and %d more", more)
	}
	return text
}

// Is makes errors.Is(err, ErrInvalidMetric) true.
func (e *InvalidMetricsError) Is(target error) bool { return target == ErrInvalidMetric }

// ValidateMetricDefinitions checks one list of metric definitions (those of
// a library, or a monitor's inline ones) against the rules the CRDs enforce
// when an object is written. It returns nil or an *InvalidMetricsError.
//
// The API server applies those rules to what is written, not to what is
// stored: an object from before a rule existed is still handed to the
// operator as it is. What MergeMetrics and the ConfigMap make of such a
// definition is not what its author meant - a version key that is no number
// is dropped, and a metric left without any statement is written out with
// none - so it is refused here, by name, instead.
//
// The rules about size (how many metrics, how many statements) are not
// repeated: they bound the cost of validation in the API server and say
// nothing about whether a definition can be rendered. Neither is the one
// about statementTimeoutSeconds, a field that is ignored.
func ValidateMetricDefinitions(defs []pgpilotv1.MetricDefinition) error {
	var problems []string
	count := make(map[string]int, len(defs))
	for i := range defs {
		md := &defs[i]
		count[md.Name]++
		if count[md.Name] == 2 {
			problems = append(problems, fmt.Sprintf("metric %s is defined more than once", quoted(md.Name)))
		}
		for _, problem := range metricProblems(md) {
			problems = append(problems, fmt.Sprintf("metric %s: %s", quoted(md.Name), problem))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return &InvalidMetricsError{Problems: problems}
}

// metricProblems returns what is wrong with one definition, in a fixed order.
func metricProblems(md *pgpilotv1.MetricDefinition) []string {
	var problems []string
	if len(md.Name) > maxMetricNameLength || !metricNamePattern.MatchString(md.Name) {
		problems = append(problems, fmt.Sprintf("the name must match %s and have at most %d characters",
			metricNamePattern, maxMetricNameLength))
	}
	if len(md.SQLs) == 0 {
		problems = append(problems, "sqls holds no statement")
	}
	versions := make([]string, 0, len(md.SQLs))
	for version := range md.SQLs {
		versions = append(versions, version)
	}
	slices.Sort(versions)
	for _, version := range versions {
		if !sqlsKeyPattern.MatchString(version) {
			problems = append(problems, fmt.Sprintf("sqls key %s is not a PostgreSQL major version written as a "+
				"plain integer, such as \"14\" (\"0\" matches every version)", quoted(version)))
		}
		if md.SQLs[version] == "" {
			problems = append(problems, fmt.Sprintf("the statement under sqls key %s is empty", quoted(version)))
		}
	}
	if md.MasterOnly && md.StandbyOnly {
		problems = append(problems, "masterOnly and standbyOnly are both set; a metric is collected on one of the two, or on both by setting neither")
	}
	if md.Interval != nil && md.Interval.Duration < minMetricInterval {
		problems = append(problems, fmt.Sprintf("interval %s is below the minimum of %s", md.Interval.Duration, minMetricInterval))
	}
	return problems
}

// quoted returns a name or a key in quotes, cut after shownCharacters
// characters, with the length of what was cut.
func quoted(s string) string {
	characters := []rune(s)
	if len(characters) <= shownCharacters {
		return strconv.Quote(s)
	}
	return fmt.Sprintf("%s... (%d characters)", strconv.Quote(string(characters[:shownCharacters])), len(characters))
}

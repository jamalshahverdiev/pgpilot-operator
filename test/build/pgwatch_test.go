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

package build

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const pgwatchData = "internal/builder/pgwatchdata"

// pgwatchVersion is the release named by the constant in the builder.
func pgwatchVersion(t *testing.T) string {
	t.Helper()
	src := readRepoFile(t, "internal/builder/pgwatch_builtin.go")
	m := regexp.MustCompile(`(?m)^\s*PgwatchVersion = "(\d+\.\d+\.\d+)"$`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("internal/builder/pgwatch_builtin.go no longer declares PgwatchVersion as a plain x.y.z constant")
	}
	return m[1]
}

// ---------------------------------------------------------------------------
// make update-pgwatch-registry
// ---------------------------------------------------------------------------

// pgwatchWorkspace is a workspace with the embedded pgwatch files and a curl
// that serves a small registry and licence instead of the network.
func pgwatchWorkspace(t *testing.T) *workspace {
	t.Helper()
	w := newWorkspace(t)
	copyDir(t, filepath.Join(repoRoot, pgwatchData), filepath.Join(w.dir, pgwatchData))
	w.script(t, w.fakeBin, "curl", `
echo "$*" >> "$FAKE_LOGS/curl.args"
out=""; prev=""; url=""
for a in "$@"; do
  if [ "$prev" = "-o" ]; then out="$a"; fi
  prev="$a"; url="$a"
done
if [ -n "${FAKE_CURL_FAIL_ON:-}" ] && [[ "$url" == *"$FAKE_CURL_FAIL_ON"* ]]; then
  echo "curl: (22) The requested URL returned error: 404" >&2
  exit 22
fi
case "$url" in
  */internal/metrics/metrics.yaml)
    if [ -n "${FAKE_CURL_GARBAGE:-}" ]; then
      printf '<html>not found</html>\n' > "$out"
    elif [ -n "${FAKE_CURL_NO_PRESETS:-}" ]; then
      printf 'metrics:\n  fetched_metric:\n    sqls:\n      14: select 1\n' > "$out"
    elif [ -n "${FAKE_CURL_NO_METRICS:-}" ]; then
      printf 'presets:\n  fetched_preset:\n    metrics:\n      fetched_metric: 60\n' > "$out"
    else
      printf 'metrics:\n  fetched_metric:\n    sqls:\n      14: select 1\n' > "$out"
      printf 'presets:\n  fetched_preset:\n    metrics:\n      fetched_metric: 60\n# %s\n' "$url" >> "$out"
    fi ;;
  */LICENSE)
    if [ -n "${FAKE_CURL_EMPTY_LICENSE:-}" ]; then
      : > "$out"
    else
      printf 'BSD 3-Clause License\n# %s\n' "$url" > "$out"
    fi ;;
  *) echo "unexpected URL $url" >&2; exit 1 ;;
esac
`)
	return w
}

func updateRegistry(t *testing.T, w *workspace, run makeRun, args ...string) (string, error) {
	t.Helper()
	full := append([]string{"update-pgwatch-registry", "CURL=" + filepath.Join(w.fakeBin, "curl")}, args...)
	return w.make(t, run, full...)
}

func pgwatchFiles(t *testing.T, w *workspace) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, name := range []string{"metrics.yaml", "LICENSE", "VERSION", "SHA256SUMS"} {
		out[name] = w.read(t, filepath.Join(pgwatchData, name))
	}
	return out
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestUpdatePgwatchRegistry_FetchesTheReleaseAndRecordsIt(t *testing.T) {
	w := pgwatchWorkspace(t)

	out, err := updateRegistry(t, w, makeRun{}, "PGWATCH_VERSION=7.8.9")
	expectSuccess(t, out, err)

	const base = "https://raw.githubusercontent.com/cybertec-postgresql/pgwatch/v7.8.9/"
	calls := w.calls(t, "curl.args")
	if len(calls) != 2 ||
		!strings.HasSuffix(calls[0], " "+base+"internal/metrics/metrics.yaml") ||
		!strings.HasSuffix(calls[1], " "+base+"LICENSE") {
		t.Fatalf("curl calls: %q", calls)
	}
	for _, call := range calls {
		// A download that fails, or is redirected to plain HTTP, must not
		// end up in the binary.
		for _, flag := range []string{"--fail", "--location", "--proto =https"} {
			if !strings.Contains(call, flag) {
				t.Errorf("curl is called without %s: %s", flag, call)
			}
		}
	}

	files := pgwatchFiles(t, w)
	if !strings.Contains(files["metrics.yaml"], "fetched_preset") || !strings.Contains(files["metrics.yaml"], base) {
		t.Errorf("metrics.yaml is not the downloaded file:\n%s", files["metrics.yaml"])
	}
	if !strings.Contains(files["LICENSE"], base+"LICENSE") {
		t.Errorf("LICENSE is not the downloaded file:\n%s", files["LICENSE"])
	}
	if files["VERSION"] != "7.8.9\n" {
		t.Errorf("VERSION = %q", files["VERSION"])
	}
	wantSums := sha256Hex(files["metrics.yaml"]) + "  metrics.yaml\n" + sha256Hex(files["LICENSE"]) + "  LICENSE\n"
	if files["SHA256SUMS"] != wantSums {
		t.Errorf("SHA256SUMS:\n got  %q\n want %q", files["SHA256SUMS"], wantSums)
	}
	if !strings.Contains(out, "Set PgwatchVersion in internal/builder/pgwatch_builtin.go") {
		t.Errorf("the recipe does not say what is left to do by hand:\n%s", out)
	}
	w.expectNoTempLeft(t)
}

// Without a version on the command line the recorded release is fetched
// again, which is how the files are verified against upstream.
func TestUpdatePgwatchRegistry_DefaultsToTheRecordedRelease(t *testing.T) {
	w := pgwatchWorkspace(t)
	recorded := strings.TrimSpace(w.read(t, filepath.Join(pgwatchData, "VERSION")))

	out, err := updateRegistry(t, w, makeRun{})
	expectSuccess(t, out, err)

	for _, call := range w.calls(t, "curl.args") {
		if !strings.Contains(call, "/v"+recorded+"/") {
			t.Errorf("curl was not asked for release %s: %s", recorded, call)
		}
	}
	if got := w.read(t, filepath.Join(pgwatchData, "VERSION")); got != recorded+"\n" {
		t.Errorf("VERSION = %q", got)
	}
}

// A half-finished update would leave a registry of one release next to the
// version or licence of another.
func TestUpdatePgwatchRegistry_ChangesNothingWhenItFails(t *testing.T) {
	release := []string{"PGWATCH_VERSION=7.8.9"}
	cases := map[string]struct {
		env  []string
		args []string
	}{
		"the registry cannot be downloaded": {env: []string{"FAKE_CURL_FAIL_ON=metrics.yaml"}, args: release},
		"the licence cannot be downloaded":  {env: []string{"FAKE_CURL_FAIL_ON=LICENSE"}, args: release},
		"the download is not a registry":    {env: []string{"FAKE_CURL_GARBAGE=1"}, args: release},
		"the registry has no presets":       {env: []string{"FAKE_CURL_NO_PRESETS=1"}, args: release},
		"the registry has no metrics":       {env: []string{"FAKE_CURL_NO_METRICS=1"}, args: release},
		"the licence is empty":              {env: []string{"FAKE_CURL_EMPTY_LICENSE=1"}, args: release},
		"the version is not a release":      {args: []string{"PGWATCH_VERSION=main"}},
		"the version carries a v":           {args: []string{"PGWATCH_VERSION=v7.8.9"}},
		"the version would change the URL":  {args: []string{"PGWATCH_VERSION=7.8.9/../../evil"}},
		"the version is empty":              {args: []string{"PGWATCH_VERSION="}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			w := pgwatchWorkspace(t)
			before := pgwatchFiles(t, w)

			out, err := updateRegistry(t, w, makeRun{env: tc.env}, tc.args...)
			expectFailure(t, out, err)

			for file, content := range pgwatchFiles(t, w) {
				if content != before[file] {
					t.Errorf("%s was changed by a failed update", file)
				}
			}
			w.expectNoTempLeft(t)
		})
	}
}

// The recorded version is read from a file and ends up in a shell command.
// Whatever the file holds has to be treated as text.
func TestUpdatePgwatchRegistry_DoesNotRunWhatTheVersionSays(t *testing.T) {
	for name, version := range map[string]string{
		"command substitution": "6.2.0$(touch INJECTED)",
		"backticks":            "6.2.0`touch INJECTED`",
		"quote and semicolon":  `6.2.0"; touch INJECTED; "`,
	} {
		t.Run(name+" in the VERSION file", func(t *testing.T) {
			w := pgwatchWorkspace(t)
			path := filepath.Join(w.dir, pgwatchData, "VERSION")
			if err := os.WriteFile(path, []byte(version+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := updateRegistry(t, w, makeRun{})
			expectFailure(t, out, err)
			if w.exists("INJECTED") || w.exists(filepath.Join(pgwatchData, "INJECTED")) {
				t.Errorf("the recipe executed text from the VERSION file:\n%s", out)
			}
			if calls := w.calls(t, "curl.args"); len(calls) != 0 {
				t.Errorf("curl was called for a version that is not a release: %q", calls)
			}
		})
		// make expands "$(...)" in a command-line value as one of its own
		// variables, so that form may come out as a plain version and
		// succeed. What must never happen is that the text is executed.
		t.Run(name+" on the command line", func(t *testing.T) {
			w := pgwatchWorkspace(t)
			out, _ := updateRegistry(t, w, makeRun{}, "PGWATCH_VERSION="+version)
			if w.exists("INJECTED") || w.exists(filepath.Join(pgwatchData, "INJECTED")) {
				t.Errorf("the recipe executed text from PGWATCH_VERSION:\n%s", out)
			}
			recorded := strings.TrimSpace(w.read(t, filepath.Join(pgwatchData, "VERSION")))
			if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(recorded) {
				t.Errorf("VERSION now holds %q", recorded)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// One pgwatch release, everywhere
// ---------------------------------------------------------------------------

func TestPgwatch_RecordedVersionAndChecksumsMatchTheFiles(t *testing.T) {
	version := pgwatchVersion(t)
	if got := readRepoFile(t, pgwatchData+"/VERSION"); got != version+"\n" {
		t.Errorf("%s/VERSION is %q, PgwatchVersion is %q", pgwatchData, got, version)
	}
	want := sha256Hex(readRepoFile(t, pgwatchData+"/metrics.yaml")) + "  metrics.yaml\n" +
		sha256Hex(readRepoFile(t, pgwatchData+"/LICENSE")) + "  LICENSE\n"
	if got := readRepoFile(t, pgwatchData+"/SHA256SUMS"); got != want {
		t.Errorf("%s/SHA256SUMS does not describe the files next to it:\n got  %q\n want %q", pgwatchData, got, want)
	}
}

// A default written into the CRD is stored by the API server in every object
// and keeps a monitor on that pgwatch release after the operator has moved
// on. The operator applies its default itself.
func TestPgwatch_CRDsDoNotDefaultTheImage(t *testing.T) {
	for _, path := range []string{
		"config/crd/bases/pgpilot.io_pgpilotmonitors.yaml",
		"charts/pgpilot-operator/files/crds/pgpilot.io_pgpilotmonitors.yaml",
	} {
		crd := parseRepoYAML(t, path)
		image := dig(t, crd, "spec", "versions", 0, "schema", "openAPIV3Schema",
			"properties", "spec", "properties", "image")
		if _, has := image["default"]; has {
			t.Errorf("%s: spec.image has a default", path)
		}
		for _, field := range []string{"repository", "tag"} {
			schema := dig(t, image, "properties", field)
			if def, has := schema["default"]; has {
				t.Errorf("%s: spec.image.%s defaults to %v", path, field, def)
			}
		}
	}
}

// dig walks nested maps and lists of parsed YAML.
func dig(t *testing.T, node any, path ...any) map[string]any {
	t.Helper()
	for _, step := range path {
		switch key := step.(type) {
		case string:
			m, ok := node.(map[string]any)
			if !ok || m[key] == nil {
				t.Fatalf("no %q on the way to %v", key, path)
			}
			node = m[key]
		case int:
			l, ok := node.([]any)
			if !ok || len(l) <= key {
				t.Fatalf("no element %d on the way to %v", key, path)
			}
			node = l[key]
		}
	}
	m, ok := node.(map[string]any)
	if !ok {
		t.Fatalf("%v is not an object", path)
	}
	return m
}

// userFacingFiles are the files people copy from: docs, samples, scripts and
// the chart's own documentation.
func userFacingFiles(t *testing.T) []string {
	t.Helper()
	files := []string{filepath.Join(repoRoot, "README.md")}
	for _, dir := range []string{"docs", "config/samples", "hack", "examples", "charts/pgpilot-operator"} {
		err := filepath.WalkDir(filepath.Join(repoRoot, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel := filepath.ToSlash(strings.TrimPrefix(path, repoRoot+string(os.PathSeparator)))
			if d.IsDir() {
				// Generated CRD copies and templates are checked elsewhere.
				if rel == "charts/pgpilot-operator/files" || rel == "charts/pgpilot-operator/templates" {
					return filepath.SkipDir
				}
				return nil
			}
			switch filepath.Ext(path) {
			case ".md", ".yaml", ".yml", ".sh", ".json", ".txt":
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(files) < 15 {
		t.Fatalf("only %d files found to check: the directories no longer match the repository", len(files))
	}
	return files
}

var (
	// "pgwatch:5.1.0", "pgwatch 5.1", "pgwatch v5.1.0", "pgwatch-5.1.0".
	pgwatchRelease = regexp.MustCompile(`(?i)pgwatch[:\s-]+v?(\d+\.\d+(?:\.\d+)?)\b`)
	// `tag: 5.1.0`, `tag: "5.1.0"`, `"tag":"5.1.0"`, `tag=5.1.0`, `- tag: '5.1.0'`.
	imageTag = regexp.MustCompile(`(?i)\btag["']?\s*[:=]\s*\\?["']?v?(\d+\.\d+\.\d+)\b`)
)

// stalePgwatchVersions returns the pgwatch versions a line of a user-facing
// file names that are not the release the operator is built for.
func stalePgwatchVersions(line, version string) []string {
	// The operator's own image and chart have versions of their own.
	if strings.Contains(line, "pgpilot-operator") {
		return nil
	}
	var stale []string
	check := func(found string) {
		// "pgwatch 6.2" is as good as "pgwatch 6.2.0".
		if found != version && !strings.HasPrefix(version, found+".") {
			stale = append(stale, found)
		}
	}
	for _, m := range pgwatchRelease.FindAllStringSubmatch(line, -1) {
		check(m[1])
	}
	for _, m := range imageTag.FindAllStringSubmatch(line, -1) {
		check(m[1])
	}
	return stale
}

// The upgrade notes and the troubleshooting guide have to name the release
// monitors come from; everywhere else a pgwatch version is a pin waiting to
// go stale.
var mayNameOldReleases = map[string]bool{
	"docs/troubleshooting.md": true,
	"docs/crd-reference.md":   true,
}

// Docs, samples and scripts must not pin a pgwatch version: whatever they
// pin is what users copy, and it goes stale with the next release.
func TestPgwatch_NothingPinsAnotherRelease(t *testing.T) {
	version := pgwatchVersion(t)

	for _, f := range userFacingFiles(t) {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		rel := filepath.ToSlash(strings.TrimPrefix(f, repoRoot+string(os.PathSeparator)))
		for i, line := range strings.Split(string(raw), "\n") {
			for _, stale := range stalePgwatchVersions(line, version) {
				if mayNameOldReleases[rel] && stale == "5.1.0" {
					continue
				}
				t.Errorf("%s:%d names pgwatch %s, the operator is built for %s: %s",
					rel, i+1, stale, version, strings.TrimSpace(line))
			}
		}
	}

	// Where the default is documented, it has to be the real one.
	reference := readRepoFile(t, "docs/crd-reference.md")
	if !strings.Contains(reference, "| `tag` | string | no | `"+version+"` |") {
		t.Errorf("docs/crd-reference.md does not give %s as the default of spec.image.tag", version)
	}
	major, _, _ := strings.Cut(version, ".")
	for _, doc := range []string{"README.md", "docs/architecture.md"} {
		if !strings.Contains(readRepoFile(t, doc), "pgwatch v"+major) {
			t.Errorf("%s does not say the operator wraps pgwatch v%s", doc, major)
		}
	}
}

// The check above is only worth something if it sees the ways a version gets
// written down.
func TestPgwatch_StaleVersionCheckSeesHowVersionsAreWritten(t *testing.T) {
	const version = "6.2.0"
	stale := []string{
		`    tag: "5.1.0"`, `    tag: 5.1.0`, `    tag: '5.1.0'`, `  - tag: "5.1.0"`,
		`image: cybertecpostgresql/pgwatch:5.1.0`, `image: cybertecpostgresql/pgwatch:5.1`,
		`runs pgwatch 5.1.0 by default`, `runs pgwatch v5.3.0`, `Pgwatch 6.1.0`, `pgwatch-6.1.0`,
		`-p '{"spec":{"image":{"tag":"5.1.0"}}}'`, `--set image.tag=5.1.0`,
		`tag: 6.2.1`,
	}
	for _, line := range stale {
		if len(stalePgwatchVersions(line, version)) == 0 {
			t.Errorf("not recognised as naming another release: %s", line)
		}
	}
	current := []string{
		`    tag: "6.2.0"`, `image: cybertecpostgresql/pgwatch:6.2.0`, `pgwatch 6.2.0`, `pgwatch 6.2`, `pgwatch v6`,
		`helm install pgpilot-operator --version 0.1.2`, `  tag: "v1.0.2" # pgpilot-operator image`,
		`PostgreSQL 14.2`, `kubeVersion: ">=1.28.0-0"`, `pgwatch is the collector`,
	}
	for _, line := range current {
		if got := stalePgwatchVersions(line, version); len(got) != 0 {
			t.Errorf("wrongly taken for another release (%v): %s", got, line)
		}
	}
}

// README and the main sample are what a first-time user applies to a
// database as it comes. The presets below contain metrics that need helper
// functions in that database, and with pgwatch 6 log an error for them at
// every collection.
func TestPgwatch_FirstExamplesWorkOnAStockDatabase(t *testing.T) {
	needsHelpers := regexp.MustCompile(`(?m)^\s*preset:\s*"?(standard|exhaustive|full|debug)"?\s*(#.*)?$`)
	for _, f := range []string{"README.md", "docs/quickstart.md", "config/samples/pgpilot_v1_pgpilotmonitor.yaml"} {
		if m := needsHelpers.FindStringSubmatch(readRepoFile(t, f)); m != nil {
			t.Errorf("%s uses preset %s, which needs pgwatch's helper functions in the database; "+
				"use basic or exhaustive_no_python", f, m[1])
		}
	}
}

// The collector job exists to run the generated configuration against the
// PostgreSQL versions it names.
func TestWorkflows_CollectorJobCoversTheSupportedPostgresVersions(t *testing.T) {
	workflow := parseRepoYAML(t, ".github/workflows/test.yml")
	matrix := dig(t, workflow, "jobs", "collector", "strategy", "matrix")
	versions, _ := matrix["postgres"].([]any)
	got := make([]string, 0, len(versions))
	for _, v := range versions {
		got = append(got, v.(string))
	}
	// The oldest version pgwatch has SQL for, the newest before its
	// PostgreSQL 19 definitions, and 19.
	if strings.Join(got, ",") != "14,18,19" {
		t.Errorf("the collector job runs PostgreSQL %v, want [14 18 19]", got)
	}

	makefile := readRepoFile(t, "Makefile")
	if !strings.Contains(makefile, `COLLECTOR_TEST_POSTGRES="$(COLLECTOR_TEST_POSTGRES)"`) {
		t.Error("make test-collector no longer hands COLLECTOR_TEST_POSTGRES to the tests")
	}
}

// The preset pattern of the CRD is why a name with a hyphen never reaches the
// operator; the docs say so.
func TestPgwatch_PresetPatternRefusesHyphens(t *testing.T) {
	crd := parseRepoYAML(t, "config/crd/bases/pgpilot.io_pgpilotmonitors.yaml")
	preset := dig(t, crd, "spec", "versions", 0, "schema", "openAPIV3Schema",
		"properties", "spec", "properties", "metrics", "properties", "preset")
	pattern, _ := preset["pattern"].(string)
	re, err := regexp.Compile(pattern)
	if err != nil || pattern == "" {
		t.Fatalf("spec.metrics.preset has no usable pattern (%q, %v)", pattern, err)
	}
	for name, want := range map[string]bool{
		"basic": true, "exhaustive_no_python": true, "patroni": true, "pgbouncer": true, "": true,
		"postgres-exporter-basic": false, "exhaustive-no-python": false, "Basic": false,
	} {
		if re.MatchString(name) != want {
			t.Errorf("preset %q: admitted=%v, want %v", name, !want, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Licence of the embedded registry
// ---------------------------------------------------------------------------

// pgwatch is BSD-3-Clause: its notice has to accompany the copy in the
// sources and in the image, where the copy is part of the binary.
func TestPgwatch_LicenceTravelsWithTheImage(t *testing.T) {
	dockerfile := readRepoFile(t, "Dockerfile")
	final := dockerfile[strings.LastIndex(dockerfile, "\nFROM "):]
	for _, want := range []string{
		"COPY LICENSE /licenses/LICENSE",
		"COPY " + pgwatchData + "/LICENSE /licenses/pgwatch.LICENSE",
	} {
		if !strings.Contains(final, want+"\n") {
			t.Errorf("the final stage of the Dockerfile lacks %q", want)
		}
	}

	// Everything is ignored by default; what the Dockerfile copies has to
	// be let back in, or the image build fails.
	ignore := readRepoFile(t, ".dockerignore")
	for _, want := range []string{"!LICENSE", "!" + pgwatchData + "/LICENSE", "!" + pgwatchData + "/metrics.yaml"} {
		if !strings.Contains("\n"+ignore+"\n", "\n"+want+"\n") {
			t.Errorf(".dockerignore does not re-include %q", strings.TrimPrefix(want, "!"))
		}
	}

	if !strings.Contains(readRepoFile(t, "README.md"), pgwatchData+"/LICENSE") {
		t.Errorf("README.md does not point at the licence of the embedded pgwatch metric definitions")
	}
}

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
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func parseRepoYAML(t *testing.T, rel string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := yaml.Unmarshal([]byte(readRepoFile(t, rel)), &out); err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	return out
}

func workflowFiles(t *testing.T) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(repoRoot, ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("no workflows found under .github/workflows")
	}
	rel := make([]string, 0, len(matches))
	for _, m := range matches {
		r, err := filepath.Rel(repoRoot, m)
		if err != nil {
			t.Fatal(err)
		}
		rel = append(rel, r)
	}
	sort.Strings(rel)
	return rel
}

// version is a dotted numeric version such as 1.27.4.
type version []int

func parseVersion(t *testing.T, s string) version {
	t.Helper()
	var v version
	for part := range strings.SplitSeq(s, ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			t.Fatalf("version %q: %v", s, err)
		}
		v = append(v, n)
	}
	return v
}

func (v version) less(other version) bool {
	for i := 0; i < len(v) && i < len(other); i++ {
		if v[i] != other[i] {
			return v[i] < other[i]
		}
	}
	return len(v) < len(other)
}

// releaseLine turns 1.27.4 into 1.27.
func releaseLine(v string) string {
	return strings.Join(strings.Split(v, ".")[:2], ".")
}

func goModVersion(t *testing.T) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^go (\d+\.\d+\.\d+)$`).FindStringSubmatch(readRepoFile(t, "go.mod"))
	if m == nil {
		t.Fatal("go.mod has no `go X.Y.Z` line")
	}
	return m[1]
}

// ---------------------------------------------------------------------------
// Go toolchain
// ---------------------------------------------------------------------------

// The builder image runs with GOTOOLCHAIN=local: when it is older than the
// `go` line of go.mod the image cannot be built at all. That is how every
// dependency update that raised the Go version used to break CI.
func TestToolchain_DockerfileBuildsWithTheGoOfGoMod(t *testing.T) {
	want := goModVersion(t)
	dockerfile := readRepoFile(t, "Dockerfile")

	from := regexp.MustCompile(`(?m)^FROM (\S+)(?: AS (\S+))?$`).FindAllStringSubmatch(dockerfile, -1)
	if len(from) != 2 {
		t.Fatalf("Dockerfile has %d FROM lines, want a builder and a final stage", len(from))
	}

	builder := regexp.MustCompile(`^golang:(\d+\.\d+\.\d+)@sha256:[0-9a-f]{64}$`).FindStringSubmatch(from[0][1])
	if builder == nil {
		t.Fatalf("builder image %q must be golang:X.Y.Z pinned by digest", from[0][1])
	}
	if from[0][2] != "builder" {
		t.Errorf("first stage is named %q, want builder", from[0][2])
	}
	if parseVersion(t, builder[1]).less(parseVersion(t, want)) {
		t.Errorf("builder image has Go %s, older than go.mod's %s", builder[1], want)
	}
	if got, wantLine := releaseLine(builder[1]), releaseLine(want); got != wantLine {
		t.Errorf("builder image is on Go %s, go.mod on %s; keep them on the same release line", got, wantLine)
	}

	if !regexp.MustCompile(`^gcr\.io/distroless/static:nonroot@sha256:[0-9a-f]{64}$`).MatchString(from[1][1]) {
		t.Errorf("final image %q must be distroless static:nonroot pinned by digest", from[1][1])
	}
}

func TestToolchain_GoVersionIsSupported(t *testing.T) {
	// Go 1.26 is the oldest release line still receiving security fixes at
	// the time of writing; raise this when a line reaches end of life.
	const oldestSupported = "1.26.0"
	if got := goModVersion(t); parseVersion(t, got).less(parseVersion(t, oldestSupported)) {
		t.Errorf("go.mod targets Go %s, which no longer receives security fixes (oldest supported: %s)",
			got, oldestSupported)
	}
}

func TestToolchain_DevcontainerUsesTheSameGoLine(t *testing.T) {
	want := releaseLine(goModVersion(t))
	devcontainer := readRepoFile(t, ".devcontainer/devcontainer.json")
	m := regexp.MustCompile(`"image":\s*"golang:([0-9.]+)"`).FindStringSubmatch(devcontainer)
	if m == nil {
		t.Fatal("devcontainer.json does not use a golang image")
	}
	if got := releaseLine(m[1]); got != want {
		t.Errorf("devcontainer uses Go %s, go.mod uses %s", got, want)
	}
}

// Tools are installed by the Makefile at pinned versions; "latest" would make
// lint and code generation results depend on the day they run.
func TestToolchain_MakefilePinsToolVersions(t *testing.T) {
	makefile := readRepoFile(t, "Makefile")
	pins := regexp.MustCompile(`(?m)^([A-Z_]+_VERSION) \?= (\S+)$`).FindAllStringSubmatch(makefile, -1)

	seen := map[string]string{}
	for _, p := range pins {
		seen[p[1]] = p[2]
		if !regexp.MustCompile(`^v?\d+\.\d+\.\d+$`).MatchString(p[2]) {
			t.Errorf("%s = %q, want a fixed X.Y.Z version", p[1], p[2])
		}
	}
	for _, name := range []string{
		"KUSTOMIZE_VERSION", "CONTROLLER_TOOLS_VERSION", "GOLANGCI_LINT_VERSION",
		"KUBECONFORM_VERSION", "GOVULNCHECK_VERSION",
	} {
		if _, ok := seen[name]; !ok {
			t.Errorf("Makefile does not pin %s", name)
		}
	}

	// A tool that analyses Go code must be built with go.mod's toolchain.
	if !strings.Contains(makefile, `GOTOOLCHAIN="$(GO_MOD_TOOLCHAIN)" GOBIN="$(LOCALBIN)" go install`) {
		t.Error("go-install-tool no longer builds tools with the toolchain of go.mod")
	}
}

// The linter never saw the e2e suite because it sits behind a build tag.
func TestToolchain_LinterCoversTheE2ESuite(t *testing.T) {
	cfg := parseRepoYAML(t, ".golangci.yml")
	run, _ := cfg["run"].(map[string]any)
	tags, _ := run["build-tags"].([]any)
	if len(tags) != 1 || tags[0] != "e2e" {
		t.Errorf(".golangci.yml run.build-tags = %v, want [e2e]", tags)
	}

	files, err := filepath.Glob(filepath.Join(repoRoot, "test", "e2e", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no e2e sources found (err: %v)", err)
	}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(f)
		if !strings.HasPrefix(string(src), "//go:build e2e\n") {
			t.Errorf("%s must start with `//go:build e2e`", name)
		}
		if strings.Contains(string(src), "// +build") {
			t.Errorf("%s still carries an old-style `// +build` line", name)
		}
		if regexp.MustCompile(`[a-z0-9/.-]+:latest\b`).Match(src) {
			t.Errorf("%s uses an image tagged :latest", name)
		}
	}
}

// ---------------------------------------------------------------------------
// CI workflows
// ---------------------------------------------------------------------------

// helmSetup matches a setup-helm step together with the helm version it pins.
var helmSetup = regexp.MustCompile(
	`azure/setup-helm@\S+ # \S+\n\s+with:\n\s+version: (v\d+\.\d+\.\d+)\n`)

var usesLine = regexp.MustCompile(`(?m)^\s*(?:- )?uses:\s*(\S+)(.*)$`)

func TestWorkflows_ActionsArePinnedByCommit(t *testing.T) {
	pinned := regexp.MustCompile(`^([\w.-]+/[\w./-]+)@([0-9a-f]{40})$`)
	comment := regexp.MustCompile(`^\s+# v\d+\.\d+\.\d+$`)

	shaByAction := map[string]string{}
	for _, wf := range workflowFiles(t) {
		uses := usesLine.FindAllStringSubmatch(readRepoFile(t, wf), -1)
		if len(uses) == 0 {
			t.Errorf("%s uses no actions at all", wf)
		}
		for _, u := range uses {
			m := pinned.FindStringSubmatch(u[1])
			if m == nil {
				t.Errorf("%s: %q must be pinned to a full commit SHA", wf, u[1])
				continue
			}
			if !comment.MatchString(u[2]) {
				t.Errorf("%s: %q needs a trailing `# vX.Y.Z` comment naming the release", wf, u[1])
			}
			if prev, ok := shaByAction[m[1]]; ok && prev != m[2] {
				t.Errorf("%s: %s is pinned to %s here and to %s elsewhere", wf, m[1], m[2], prev)
			}
			shaByAction[m[1]] = m[2]
		}
	}
}

func TestWorkflows_AreLeastPrivilegeAndBounded(t *testing.T) {
	for _, wf := range workflowFiles(t) {
		t.Run(filepath.Base(wf), func(t *testing.T) {
			doc := parseRepoYAML(t, wf)

			perms, _ := doc["permissions"].(map[string]any)
			if len(perms) != 1 || perms["contents"] != "read" {
				t.Errorf("permissions = %v, want exactly {contents: read}", doc["permissions"])
			}

			conc, _ := doc["concurrency"].(map[string]any)
			if conc["cancel-in-progress"] != true || conc["group"] == nil {
				t.Errorf("concurrency = %v, want a group with cancel-in-progress", doc["concurrency"])
			}

			jobs, _ := doc["jobs"].(map[string]any)
			if len(jobs) == 0 {
				t.Fatal("workflow has no jobs")
			}
			for name, j := range jobs {
				job, _ := j.(map[string]any)
				if minutes, ok := job["timeout-minutes"].(float64); !ok || minutes <= 0 || minutes > 60 {
					t.Errorf("job %s: timeout-minutes = %v, want 1..60", name, job["timeout-minutes"])
				}
				if _, ok := job["permissions"]; ok {
					t.Errorf("job %s overrides the workflow permissions", name)
				}
			}
		})
	}
}

// Every workflow used to run on every push as well as on pull_request, so a
// pull request from a branch of this repository ran everything twice.
func TestWorkflows_RunOnPullRequestsAndMainOnly(t *testing.T) {
	for _, wf := range workflowFiles(t) {
		t.Run(filepath.Base(wf), func(t *testing.T) {
			doc := parseRepoYAML(t, wf)
			// YAML 1.1 reads the key `on` as the boolean true.
			on, ok := doc["on"].(map[string]any)
			if !ok {
				on, _ = doc["true"].(map[string]any)
			}
			if on == nil {
				t.Fatal("workflow has no `on` block")
			}
			if _, ok := on["pull_request"]; !ok {
				t.Error("workflow does not run on pull_request")
			}
			push, _ := on["push"].(map[string]any)
			branches, _ := push["branches"].([]any)
			if len(branches) != 1 || branches[0] != "main" {
				t.Errorf("push.branches = %v, want [main]", push["branches"])
			}
		})
	}
}

func TestWorkflows_RunTheMakefileTargets(t *testing.T) {
	want := map[string][]string{
		".github/workflows/lint.yml": {
			"run: make lint-config",
			"run: make lint",
			"run: helm lint charts/pgpilot-operator/ --strict",
			"run: make helm-validate",
		},
		".github/workflows/test.yml": {
			"run: go mod tidy -diff",
			"run: make test",
			`git status --porcelain`,
		},
		".github/workflows/test-e2e.yml": {
			"run: make test-e2e",
			"sha256sum --check --strict",
		},
		".github/workflows/govulncheck.yml": {
			"run: make govulncheck",
			"schedule:",
		},
	}
	files := workflowFiles(t)
	for wf, lines := range want {
		found := false
		for _, f := range files {
			if filepath.ToSlash(f) == wf {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is missing", wf)
			continue
		}
		content := readRepoFile(t, wf)
		for _, line := range lines {
			if !strings.Contains(content, line) {
				t.Errorf("%s no longer contains %q", wf, line)
			}
		}
	}
}

func TestWorkflows_DoNotUseFloatingVersions(t *testing.T) {
	forbidden := map[string]string{
		"golangci-lint-action": "the linter version is pinned in the Makefile; run `make lint`",
		"/dl/latest/":          "download a pinned kind release and verify its checksum",
		":latest":              "pin image tags",
		"@latest":              "pin tool versions in the Makefile",
		"go mod tidy\n":        "CI must test the committed go.mod; use `go mod tidy -diff` to check it",
	}
	helmVersions := map[string]bool{}
	for _, wf := range workflowFiles(t) {
		content := readRepoFile(t, wf)
		for needle, why := range forbidden {
			if strings.Contains(content, needle) {
				t.Errorf("%s contains %q: %s", wf, strings.TrimSpace(needle), why)
			}
		}
		if strings.Contains(content, "azure/setup-helm@") {
			m := helmSetup.FindStringSubmatch(content)
			if m == nil {
				t.Errorf("%s sets up helm without pinning its version", wf)
				continue
			}
			helmVersions[m[1]] = true
		}
	}
	if len(helmVersions) != 1 {
		t.Errorf("workflows must agree on one helm version, found %v", helmVersions)
	}

	e2e := readRepoFile(t, ".github/workflows/test-e2e.yml")
	if !regexp.MustCompile(`KIND_VERSION: v\d+\.\d+\.\d+\n`).MatchString(e2e) ||
		!regexp.MustCompile(`KIND_SHA256: [0-9a-f]{64}\n`).MatchString(e2e) {
		t.Error("test-e2e.yml must pin kind by version and sha256")
	}
}

// ---------------------------------------------------------------------------
// Dependabot
// ---------------------------------------------------------------------------

// The Dockerfile, go.mod and the devcontainer have to move to a new Go release
// line together (see TestToolchain_*). Dependabot can only change the
// Dockerfile, so a minor bump from it would be a pull request that cannot pass.
func TestDependabot_LeavesGoReleaseLineBumpsToAHuman(t *testing.T) {
	doc := parseRepoYAML(t, ".github/dependabot.yml")
	updates, _ := doc["updates"].([]any)

	for _, u := range updates {
		entry, _ := u.(map[string]any)
		if entry["package-ecosystem"] != "docker" {
			continue
		}
		ignore, _ := entry["ignore"].([]any)
		for _, i := range ignore {
			rule, _ := i.(map[string]any)
			if rule["dependency-name"] != "golang" {
				continue
			}
			types, _ := rule["update-types"].([]any)
			got := map[any]bool{}
			for _, ty := range types {
				got[ty] = true
			}
			if !got["version-update:semver-minor"] || !got["version-update:semver-major"] {
				t.Errorf("golang ignore rule = %v, want minor and major updates ignored", types)
			}
			if got["version-update:semver-patch"] {
				t.Error("patch releases of the golang image must still be proposed")
			}
			return
		}
		t.Fatal("the docker ecosystem has no ignore rule for the golang image")
	}
	t.Fatal("dependabot does not watch the docker ecosystem")
}

func TestDependabot_CoversIndirectModulesAndAllEcosystems(t *testing.T) {
	doc := parseRepoYAML(t, ".github/dependabot.yml")
	updates, _ := doc["updates"].([]any)

	byEcosystem := map[string]map[string]any{}
	for _, u := range updates {
		entry, _ := u.(map[string]any)
		name, _ := entry["package-ecosystem"].(string)
		byEcosystem[name] = entry
	}
	for _, eco := range []string{"gomod", "github-actions", "docker"} {
		if byEcosystem[eco] == nil {
			t.Errorf("dependabot does not watch the %s ecosystem", eco)
		}
	}

	gomod := byEcosystem["gomod"]
	if gomod == nil {
		t.FailNow()
	}
	// Without this the vulnerable modules the operator links only
	// transitively (grpc, x/net, OpenTelemetry) never get an update.
	allow, _ := gomod["allow"].([]any)
	allowsAll := false
	for _, a := range allow {
		if rule, _ := a.(map[string]any); rule["dependency-type"] == "all" {
			allowsAll = true
		}
	}
	if !allowsAll {
		t.Errorf("gomod updates must allow dependency-type all, got %v", gomod["allow"])
	}

	groups, _ := gomod["groups"].(map[string]any)
	validKeys := map[string]bool{
		"patterns": true, "exclude-patterns": true, "dependency-type": true,
		"update-types": true, "applies-to": true,
	}
	for name, g := range groups {
		group, _ := g.(map[string]any)
		for key := range group {
			if !validKeys[key] {
				t.Errorf("group %s uses unknown key %q", name, key)
			}
		}
		if patterns, _ := group["patterns"].([]any); len(patterns) == 0 {
			t.Errorf("group %s matches nothing: it has no patterns", name)
		}
	}

	k8s, _ := groups["k8s"].(map[string]any)
	patterns, _ := k8s["patterns"].([]any)
	for _, want := range []string{"k8s.io/*", "sigs.k8s.io/*", "github.com/prometheus-operator/*"} {
		found := false
		for _, p := range patterns {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Errorf("k8s group must include %s so the libraries that move together arrive together", want)
		}
	}

	// A dependency joins the first group that matches; the catch-all has to
	// come after the specific ones.
	raw := readRepoFile(t, ".github/dependabot.yml")
	catchAll := strings.Index(raw, "      other:")
	if catchAll < 0 {
		t.Fatal("dependabot has no catch-all `other` group")
	}
	for _, specific := range []string{"      k8s:", "      test:"} {
		if i := strings.Index(raw, specific); i < 0 || i > catchAll {
			t.Errorf("group %q must be declared before the catch-all group", strings.TrimSpace(specific))
		}
	}
}

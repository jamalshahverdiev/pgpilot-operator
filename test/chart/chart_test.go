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

// Package chart holds tests for the Helm chart under charts/pgpilot-operator:
// that its CRD copy matches the generated CRDs, that it renders each object
// exactly once, and that the versions it references agree with the rest of
// the repository.
//
// Tests that render the chart need the `helm` binary. They are skipped when
// it is missing on a developer machine and fail when it is missing in CI.
package chart

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const (
	crdKind        = "CustomResourceDefinition"
	keepAnnotation = "helm.sh/resource-policy"
	// kubeVersion is passed to `helm template`, which otherwise assumes an
	// old cluster version and trips the chart's kubeVersion constraint.
	kubeVersion = "1.31.0"
)

var (
	repoRoot     = filepath.Join("..", "..")
	chartDir     = filepath.Join(repoRoot, "charts", "pgpilot-operator")
	chartCRDDir  = filepath.Join(chartDir, "files", "crds")
	generatedCRD = filepath.Join(repoRoot, "config", "crd", "bases")

	docSeparator = regexp.MustCompile(`(?m)^---\s*$`)
	// Chart versions are pushed as OCI tags next to the image tags, so they
	// must be strict semver without a "v" prefix (image tags carry the "v").
	strictSemver = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)
	imageTagForm = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)
)

type object = map[string]any

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func parseYAML(t *testing.T, data []byte, what string) object {
	t.Helper()
	var obj object
	if err := yaml.Unmarshal(data, &obj); err != nil {
		t.Fatalf("parse %s: %v", what, err)
	}
	return obj
}

func yamlFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".yaml") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

func nested(obj object, keys ...string) any {
	var cur any = obj
	for _, k := range keys {
		m, ok := cur.(object)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}

func nestedString(obj object, keys ...string) string {
	s, _ := nested(obj, keys...).(string)
	return s
}

func requireHelm(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("helm")
	if err == nil {
		return path
	}
	if os.Getenv("CI") != "" {
		t.Fatalf("helm is required in CI but was not found: %v", err)
	}
	t.Skip("helm not found in PATH; skipping chart rendering test")
	return ""
}

// render runs `helm template` with the given extra arguments and returns the
// rendered objects.
func render(t *testing.T, extraArgs ...string) []object {
	t.Helper()
	helm := requireHelm(t)
	trackChartFiles(t)

	args := append([]string{
		"template", "release", chartDir,
		"--namespace", "pgpilot-system",
		"--kube-version", kubeVersion,
	}, extraArgs...)

	var stdout, stderr bytes.Buffer
	cmd := exec.Command(helm, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}

	docs := docSeparator.Split(stdout.String(), -1)
	objs := make([]object, 0, len(docs))
	for i, doc := range docs {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		obj := parseYAML(t, []byte(doc), fmt.Sprintf("rendered document %d", i))
		if len(obj) == 0 {
			continue // comment-only document
		}
		objs = append(objs, obj)
	}
	if len(objs) == 0 {
		t.Fatalf("helm %s rendered no objects", strings.Join(args, " "))
	}
	return objs
}

// trackChartFiles stats every file of the chart. helm reads them in a child
// process, which Go's test cache cannot see; without this a template-only
// edit would be answered with a cached "ok".
func trackChartFiles(t *testing.T) {
	t.Helper()
	err := filepath.WalkDir(chartDir, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		_, err = os.Stat(path)
		return err
	})
	if err != nil {
		t.Fatalf("walk %s: %v", chartDir, err)
	}
}

func ofKind(objs []object, kind string) []object {
	var out []object
	for _, o := range objs {
		if nestedString(o, "kind") == kind {
			out = append(out, o)
		}
	}
	return out
}

func generatedCRDs(t *testing.T) map[string]object {
	t.Helper()
	out := map[string]object{}
	for _, name := range yamlFiles(t, generatedCRD) {
		obj := parseYAML(t, readFile(t, filepath.Join(generatedCRD, name)), name)
		out[nestedString(obj, "metadata", "name")] = obj
	}
	if len(out) == 0 {
		t.Fatalf("no generated CRDs found in %s", generatedCRD)
	}
	return out
}

func chartMetadata(t *testing.T) object {
	t.Helper()
	return parseYAML(t, readFile(t, filepath.Join(chartDir, "Chart.yaml")), "Chart.yaml")
}

// valueSets are the toggle combinations every structural check runs against.
var valueSets = map[string][]string{
	"defaults":                nil,
	"installCRDs=false":       {"--set", "installCRDs=false"},
	"rbac.create=false":       {"--set", "rbac.create=false"},
	"leaderElection disabled": {"--set", "leaderElection.enabled=false"},
	"metrics disabled":        {"--set", "metrics.enabled=false"},
	"metrics insecure":        {"--set", "metrics.secure=false"},
	"serviceAccount not made": {"--set", "serviceAccount.create=false", "--set", "serviceAccount.name=existing"},
	"everything optional off": {
		"--set", "installCRDs=false,rbac.create=false",
		"--set", "leaderElection.enabled=false,metrics.enabled=false",
	},
	"fullnameOverride":             {"--set", "fullnameOverride=custom-name"},
	"credentials recheck interval": {"--set", "credentials.recheckInterval=1m"},
	"collector user":               {"--set", "collector.runAsUser=none"},
	"stored image tag kept":        {"--set", "migrations.storedImageTag=false"},
	"image tag override":           {"--set", "image.tag=v9.9.9"},
	"image repository override":    {"--set", "image.repository=registry.example.com/mirror/pgpilot-operator"},
}

// ---------------------------------------------------------------------------
// CRD copy in the chart
// ---------------------------------------------------------------------------

// The chart's CRDs are copied from config/crd/bases by `make manifests`. A
// stale copy means Helm users get a different API than the one the operator
// was built against.
func TestChartCRDs_MatchGenerated(t *testing.T) {
	want := yamlFiles(t, generatedCRD)
	got := yamlFiles(t, chartCRDDir)

	if len(want) == 0 {
		t.Fatalf("no generated CRDs in %s", generatedCRD)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("chart CRD files differ from generated ones; run `make manifests`\n chart:     %v\n generated: %v",
			got, want)
	}
	for _, name := range want {
		a := readFile(t, filepath.Join(generatedCRD, name))
		b := readFile(t, filepath.Join(chartCRDDir, name))
		if !bytes.Equal(a, b) {
			t.Errorf("%s in the chart is out of date; run `make manifests`", name)
		}
	}
}

// Helm installs everything under <chart>/crds on its own, before and
// independently of the templates. Shipping CRDs there as well as through the
// templates makes `helm install` fail with an ownership conflict.
func TestChart_HasNoHelmCRDsDirectory(t *testing.T) {
	legacy := filepath.Join(chartDir, "crds")
	if _, err := os.Stat(legacy); err == nil {
		t.Fatalf("%s must not exist: CRDs are rendered from files/crds by templates/crds/crds.yaml", legacy)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", legacy, err)
	}
}

func TestChartCRDs_AreValidCRDManifests(t *testing.T) {
	for _, name := range yamlFiles(t, chartCRDDir) {
		obj := parseYAML(t, readFile(t, filepath.Join(chartCRDDir, name)), name)
		if got := nestedString(obj, "kind"); got != crdKind {
			t.Errorf("%s: kind = %q, want %q", name, got, crdKind)
		}
		if got := nestedString(obj, "apiVersion"); got != "apiextensions.k8s.io/v1" {
			t.Errorf("%s: apiVersion = %q", name, got)
		}
		if got := nestedString(obj, "spec", "group"); got != "pgpilot.io" {
			t.Errorf("%s: spec.group = %q, want pgpilot.io", name, got)
		}
	}
}

// `kubectl apply` without --server-side stores the whole object once more in
// an annotation, and the API server takes at most 262144 bytes of
// annotations: a CRD beyond that can only be applied server-side, and a
// plain `kubectl apply -f` of the installation manifest fails. The schemas
// of pod fields (affinity above all) are most of the monitor's CRD.
func TestChartCRDs_FitAClientSideApply(t *testing.T) {
	const limit = 262144
	for _, name := range yamlFiles(t, chartCRDDir) {
		obj := parseYAML(t, readFile(t, filepath.Join(chartCRDDir, name)), name)
		encoded, err := json.Marshal(obj)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// Room for what is not in the file: the annotations and labels a
		// chart or kustomize adds.
		if len(encoded) > limit*3/4 {
			t.Errorf("%s is %d bytes as JSON, more than three quarters of the %d a client-side apply can carry; "+
				"shorten descriptions (controller-gen crd:maxDescLen) or leave a pod field out", name, len(encoded), limit)
		}
	}
}

// ---------------------------------------------------------------------------
// rendering
// ---------------------------------------------------------------------------

func TestRender_DefaultValues_InstallEachCRDExactlyOnce(t *testing.T) {
	want := generatedCRDs(t)
	rendered := ofKind(render(t), crdKind)

	seen := map[string]int{}
	for _, crd := range rendered {
		seen[nestedString(crd, "metadata", "name")]++
	}
	for name := range want {
		if seen[name] != 1 {
			t.Errorf("CRD %s rendered %d times, want exactly 1", name, seen[name])
		}
	}
	for name := range seen {
		if _, ok := want[name]; !ok {
			t.Errorf("chart renders unexpected CRD %s", name)
		}
	}
}

// Templating must not alter the schema: what Helm applies has to be the CRD
// controller-gen produced.
func TestRender_CRDsAreUnmodifiedApartFromAnnotations(t *testing.T) {
	want := generatedCRDs(t)

	for _, crd := range ofKind(render(t), crdKind) {
		name := nestedString(crd, "metadata", "name")
		src, ok := want[name]
		if !ok {
			continue // reported by the exactly-once test
		}
		if !reflect.DeepEqual(crd["spec"], src["spec"]) {
			t.Errorf("CRD %s: rendered spec differs from config/crd/bases", name)
		}
		for _, key := range []string{"apiVersion", "kind"} {
			if !reflect.DeepEqual(crd[key], src[key]) {
				t.Errorf("CRD %s: %s = %v, want %v", name, key, crd[key], src[key])
			}
		}

		srcAnnotations, _ := nested(src, "metadata", "annotations").(object)
		gotAnnotations, _ := nested(crd, "metadata", "annotations").(object)
		for k, v := range srcAnnotations {
			if gotAnnotations[k] != v {
				t.Errorf("CRD %s: annotation %s = %v, want %v (generated annotations must survive)", name, k, gotAnnotations[k], v)
			}
		}
		if len(gotAnnotations) != len(srcAnnotations)+1 {
			t.Errorf("CRD %s: annotations = %v, want the generated ones plus %s", name, gotAnnotations, keepAnnotation)
		}
	}
}

// Without the keep policy `helm uninstall` deletes the CRDs and, with them,
// every PgpilotMonitor and PgpilotMetricLibrary in the cluster.
func TestRender_CRDsAreKeptOnUninstall(t *testing.T) {
	crds := ofKind(render(t), crdKind)
	if len(crds) == 0 {
		t.Fatal("no CRDs rendered with default values")
	}
	for _, crd := range crds {
		name := nestedString(crd, "metadata", "name")
		if got := nestedString(crd, "metadata", "annotations", keepAnnotation); got != "keep" {
			t.Errorf("CRD %s: %s = %q, want \"keep\"", name, keepAnnotation, got)
		}
	}
}

func TestRender_InstallCRDsFalse_RendersNoCRDs(t *testing.T) {
	objs := render(t, "--set", "installCRDs=false")

	if crds := ofKind(objs, crdKind); len(crds) != 0 {
		t.Errorf("installCRDs=false rendered %d CRDs, want 0", len(crds))
	}
	if deps := ofKind(objs, "Deployment"); len(deps) != 1 {
		t.Errorf("installCRDs=false rendered %d Deployments, want 1", len(deps))
	}
}

// A duplicate object in one release is what broke `helm install` before:
// every (apiVersion, kind, namespace, name) must be unique.
func TestRender_NoDuplicateObjects(t *testing.T) {
	for name, args := range valueSets {
		t.Run(name, func(t *testing.T) {
			seen := map[string]bool{}
			for _, o := range render(t, args...) {
				key := strings.Join([]string{
					nestedString(o, "apiVersion"),
					nestedString(o, "kind"),
					nestedString(o, "metadata", "namespace"),
					nestedString(o, "metadata", "name"),
				}, "/")
				if seen[key] {
					t.Errorf("object rendered more than once: %s", key)
				}
				seen[key] = true
			}
		})
	}
}

func TestRender_EveryObjectIsWellFormed(t *testing.T) {
	for name, args := range valueSets {
		t.Run(name, func(t *testing.T) {
			for i, o := range render(t, args...) {
				for _, field := range [][]string{{"apiVersion"}, {"kind"}, {"metadata", "name"}} {
					if nestedString(o, field...) == "" {
						t.Errorf("document %d has no %s: %v", i, strings.Join(field, "."), o)
					}
				}
			}
		})
	}
}

func managerImage(t *testing.T, objs []object) string {
	t.Helper()
	deps := ofKind(objs, "Deployment")
	if len(deps) != 1 {
		t.Fatalf("rendered %d Deployments, want 1", len(deps))
	}
	containers, _ := nested(deps[0], "spec", "template", "spec", "containers").([]any)
	if len(containers) != 1 {
		t.Fatalf("manager Deployment has %d containers, want 1", len(containers))
	}
	c, _ := containers[0].(object)
	return nestedString(c, "image")
}

func TestRender_ManagerImage(t *testing.T) {
	appVersion := nestedString(chartMetadata(t), "appVersion")
	values := parseYAML(t, readFile(t, filepath.Join(chartDir, "values.yaml")), "values.yaml")
	repo := nestedString(values, "image", "repository")
	if repo == "" {
		t.Fatal("values.yaml has no image.repository")
	}

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "default tag is the chart appVersion", want: repo + ":" + appVersion},
		{name: "tag override", args: []string{"--set", "image.tag=v9.9.9"}, want: repo + ":v9.9.9"},
		{
			name: "repository override keeps the default tag",
			args: []string{"--set", "image.repository=registry.example.com/mirror/op"},
			want: "registry.example.com/mirror/op:" + appVersion,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := managerImage(t, render(t, tt.args...)); got != tt.want {
				t.Errorf("image = %q, want %q", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// versions
// ---------------------------------------------------------------------------

// The image is published as "vX.Y.Z" (the git tag). The chart's default image
// tag and the kustomize deployment must both point at that exact tag,
// otherwise one of the two install paths cannot pull its image.
func TestVersions_ImageTagIsConsistent(t *testing.T) {
	appVersion := nestedString(chartMetadata(t), "appVersion")
	if !imageTagForm.MatchString(appVersion) {
		t.Errorf("Chart.yaml appVersion = %q, want the image tag form vX.Y.Z", appVersion)
	}

	kustomization := parseYAML(t,
		readFile(t, filepath.Join(repoRoot, "config", "manager", "kustomization.yaml")),
		"config/manager/kustomization.yaml")
	images, _ := kustomization["images"].([]any)
	if len(images) != 1 {
		t.Fatalf("config/manager/kustomization.yaml has %d image entries, want 1", len(images))
	}
	image, _ := images[0].(object)
	if got := nestedString(image, "newTag"); got != appVersion {
		t.Errorf("kustomize newTag = %q, chart appVersion = %q; they must be identical", got, appVersion)
	}

	values := parseYAML(t, readFile(t, filepath.Join(chartDir, "values.yaml")), "values.yaml")
	if got, want := nestedString(image, "newName"), nestedString(values, "image", "repository"); got != want {
		t.Errorf("kustomize image %q differs from the chart's image.repository %q", got, want)
	}
}

// The top CHANGELOG entry names the release being prepared. Forgetting to bump
// the chart for it would publish the previous image under a new changelog.
func TestVersions_MatchChangelog(t *testing.T) {
	meta := chartMetadata(t)
	appVersion := nestedString(meta, "appVersion")
	chartVersion := nestedString(meta, "version")

	changelog := string(readFile(t, filepath.Join(repoRoot, "CHANGELOG.md")))
	headings := regexp.MustCompile(`(?m)^## (v\d+\.\d+\.\d+\S*)\s*$`).FindAllStringSubmatchIndex(changelog, 2)
	if len(headings) == 0 {
		t.Fatal("CHANGELOG.md has no `## vX.Y.Z` heading")
	}
	latest := changelog[headings[0][2]:headings[0][3]]
	if latest != appVersion {
		t.Errorf("latest CHANGELOG entry is %s but Chart.yaml appVersion is %s", latest, appVersion)
	}

	entry := changelog[headings[0][1]:]
	if len(headings) > 1 {
		entry = changelog[headings[0][1]:headings[1][0]]
	}
	if want := "Helm chart `" + chartVersion + "`"; !strings.Contains(entry, want) {
		t.Errorf("CHANGELOG entry for %s must name the chart version as %q", latest, want)
	}
}

// Chart and image share one OCI repository. Chart versions therefore must be
// plain semver and image tags "v"-prefixed, so a version lookup can never
// resolve to the wrong artifact.
func TestVersions_ChartVersionCannotCollideWithImageTags(t *testing.T) {
	meta := chartMetadata(t)
	version := nestedString(meta, "version")
	if !strictSemver.MatchString(version) {
		t.Errorf("Chart.yaml version = %q, want plain semver without a v prefix", version)
	}
	if version == nestedString(meta, "appVersion") {
		t.Errorf("chart version and appVersion are both %q; they would share an OCI tag", version)
	}
}

// Installing without --version lets Helm pick whatever tag sorts highest in
// the shared repository. The documented commands must pin the chart version
// and it must be the version of the chart in this tree.
func TestDocs_InstallCommandPinsChartVersion(t *testing.T) {
	version := nestedString(chartMetadata(t), "version")
	install := regexp.MustCompile(`(?s)helm (?:install|upgrade)[^\n]*oci://.*?\n\x60\x60\x60`)

	for _, doc := range []string{"README.md", filepath.Join("docs", "quickstart.md")} {
		t.Run(doc, func(t *testing.T) {
			content := string(readFile(t, filepath.Join(repoRoot, doc)))
			blocks := install.FindAllString(content, -1)
			if len(blocks) == 0 {
				t.Fatalf("%s documents no `helm install ... oci://` command", doc)
			}
			for _, block := range blocks {
				if !strings.Contains(block, "--version "+version) {
					t.Errorf("%s: install command does not pin --version %s:\n%s", doc, version, block)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// manager arguments
// ---------------------------------------------------------------------------

func managerArgs(t *testing.T, objs []object) []string {
	t.Helper()
	deps := ofKind(objs, "Deployment")
	if len(deps) != 1 {
		t.Fatalf("rendered %d Deployments, want 1", len(deps))
	}
	containers, _ := nested(deps[0], "spec", "template", "spec", "containers").([]any)
	c, _ := containers[0].(object)
	raw, _ := c["args"].([]any)
	args := make([]string, 0, len(raw))
	for _, a := range raw {
		args = append(args, a.(string))
	}
	return args
}

// Leader election is what keeps the operator pod of the old release from
// working next to the one of the new release during an upgrade. Without it
// the old pod has to be gone before the new one starts: an operator of an
// earlier release puts back, for one, the finalizer this release takes off.
func TestRender_WithoutLeaderElectionTheOperatorIsReplacedNotRolled(t *testing.T) {
	strategy := func(args ...string) any {
		deps := ofKind(render(t, args...), "Deployment")
		if len(deps) != 1 {
			t.Fatalf("rendered %d Deployments, want 1", len(deps))
		}
		return nested(deps[0], "spec", "strategy")
	}
	if got := strategy(); got != nil {
		t.Errorf("with leader election the strategy is left to the default, got %v", got)
	}
	got, _ := strategy("--set", "leaderElection.enabled=false").(object)
	if len(got) != 1 || got["type"] != "Recreate" {
		t.Errorf("without leader election: strategy %v, want only type Recreate", got)
	}
}

// The interval at which credential Secrets are read again is an operator
// flag; chart users can only set it through this value.
func TestRender_CredentialsRecheckInterval(t *testing.T) {
	const flag = "--credentials-recheck-interval"
	count := func(args []string) (n int, value string) {
		for _, a := range args {
			if strings.HasPrefix(a, flag) {
				n++
				value = a
			}
		}
		return n, value
	}

	t.Run("left to the operator's default when unset", func(t *testing.T) {
		if n, got := count(managerArgs(t, render(t))); n != 0 {
			t.Errorf("default values render %q; the flag must be absent so the operator's default applies", got)
		}
	})

	for _, interval := range []string{"1m", "30s", "1h30m"} {
		t.Run("set to "+interval, func(t *testing.T) {
			args := managerArgs(t, render(t, "--set", "credentials.recheckInterval="+interval))
			n, got := count(args)
			if n != 1 || got != flag+"="+interval {
				t.Errorf("args = %v, want exactly one %s=%s", args, flag, interval)
			}
		})
	}

	t.Run("does not disturb the other arguments", func(t *testing.T) {
		with := managerArgs(t, render(t, "--set", "credentials.recheckInterval=1m"))
		without := managerArgs(t, render(t))
		var rest []string
		for _, a := range with {
			if !strings.HasPrefix(a, flag) {
				rest = append(rest, a)
			}
		}
		if strings.Join(rest, " ") != strings.Join(without, " ") {
			t.Errorf("other arguments changed:\n with:    %v\n without: %v", rest, without)
		}
	})
}

// Whether the operator removes the pgwatch image tag an earlier release wrote
// into the monitors is an operator flag that defaults to on; chart users can
// only turn it off through this value.
func TestRender_StoredImageTagMigration(t *testing.T) {
	const flag = "--migrate-stored-image-tag"
	find := func(args []string) []string {
		var found []string
		for _, a := range args {
			if strings.HasPrefix(a, flag) {
				found = append(found, a)
			}
		}
		return found
	}

	t.Run("left on by default", func(t *testing.T) {
		if got := find(managerArgs(t, render(t))); len(got) != 0 {
			t.Errorf("default values render %q; the flag must be absent so the operator's default (on) applies", got)
		}
	})
	t.Run("on when set explicitly", func(t *testing.T) {
		if got := find(managerArgs(t, render(t, "--set", "migrations.storedImageTag=true"))); len(got) != 0 {
			t.Errorf("storedImageTag=true renders %q", got)
		}
	})
	// A values file or --set-string gives "false" as text; that must not
	// read as "on".
	t.Run("turned off with the value given as text", func(t *testing.T) {
		got := find(managerArgs(t, render(t, "--set-string", "migrations.storedImageTag=false")))
		if len(got) != 1 || got[0] != flag+"=false" {
			t.Errorf("args with the string \"false\": %v, want %s=false", got, flag)
		}
	})
	// --reuse-values on a release installed before the value existed.
	t.Run("on when the whole block is missing", func(t *testing.T) {
		if got := find(managerArgs(t, render(t, "--set", "migrations=null"))); len(got) != 0 {
			t.Errorf("without a migrations block the chart renders %q", got)
		}
	})
	t.Run("turned off", func(t *testing.T) {
		with := managerArgs(t, render(t, "--set", "migrations.storedImageTag=false"))
		if got := find(with); len(got) != 1 || got[0] != flag+"=false" {
			t.Errorf("args = %v, want exactly one %s=false", with, flag)
		}
		var rest []string
		for _, a := range with {
			if !strings.HasPrefix(a, flag) {
				rest = append(rest, a)
			}
		}
		if without := managerArgs(t, render(t)); strings.Join(rest, " ") != strings.Join(without, " ") {
			t.Errorf("other arguments changed:\n with:    %v\n without: %v", rest, without)
		}
	})
}

// renderFailure runs `helm template` with arguments it is expected to refuse
// and returns what helm said.
func renderFailure(t *testing.T, extraArgs ...string) string {
	t.Helper()
	helm := requireHelm(t)
	args := append([]string{
		"template", "release", chartDir, "--namespace", "pgpilot-system", "--kube-version", kubeVersion,
	}, extraArgs...)
	out, err := exec.Command(helm, args...).CombinedOutput()
	if err == nil {
		t.Fatalf("helm %s rendered the chart; it should have refused", strings.Join(args, " "))
	}
	return string(out)
}

// The user of the collector containers is a property of the cluster, and so
// a value of the chart. Left alone the operator decides: 65534, or none on
// OpenShift.
func TestRender_CollectorRunAsUser(t *testing.T) {
	const flag = "--collector-run-as-user"
	find := func(args []string) []string {
		var found []string
		for _, a := range args {
			if strings.HasPrefix(a, flag) {
				found = append(found, a)
			}
		}
		return found
	}

	for name, set := range map[string][]string{
		"by default":                 {},
		"auto, said out loud":        {"--set", "collector.runAsUser=auto"},
		"an empty value":             {"--set-string", "collector.runAsUser="},
		"the whole block is missing": {"--set", "collector=null"}, // --reuse-values from before the value existed
	} {
		t.Run(name, func(t *testing.T) {
			if got := find(managerArgs(t, render(t, set...))); len(got) != 0 {
				t.Errorf("renders %q; the flag must be absent so that the operator decides", got)
			}
		})
	}

	for name, tc := range map[string]struct {
		set  []string
		want string
	}{
		"a user, as a number": {[]string{"--set", "collector.runAsUser=1001"}, flag + "=1001"},
		"a user, as text":     {[]string{"--set-string", "collector.runAsUser=1001"}, flag + "=1001"},
		"a user of OpenShift": {[]string{"--set", "collector.runAsUser=1000680000"}, flag + "=1000680000"},
		"none":                {[]string{"--set", "collector.runAsUser=none"}, flag + "=none"},
		// A values file, --set-json and the stored values of a release
		// (helm upgrade --reuse-values) give a number as a float, which
		// prints as 1.00068e+09 unless it is told not to.
		"a user of OpenShift, from JSON": {
			[]string{"--set-json", `collector={"runAsUser":1000680000}`}, flag + "=1000680000",
		},
		"a user of OpenShift, from a file": {
			[]string{"-f", valuesFile(t, "collector:\n  runAsUser: 1000680000\n")}, flag + "=1000680000",
		},
		"a quoted user, from a file": {
			[]string{"-f", valuesFile(t, "collector:\n  runAsUser: \"1001\"\n")}, flag + "=1001",
		},
		"none, from a file": {[]string{"-f", valuesFile(t, "collector:\n  runAsUser: none\n")}, flag + "=none"},
		"the largest user there is": {
			[]string{"--set-json", `collector={"runAsUser":2147483647}`}, flag + "=2147483647",
		},
	} {
		t.Run(name, func(t *testing.T) {
			with := managerArgs(t, render(t, tc.set...))
			if got := find(with); len(got) != 1 || got[0] != tc.want {
				t.Errorf("args = %v, want exactly one %s", with, tc.want)
			}
			var rest []string
			for _, a := range with {
				if !strings.HasPrefix(a, flag) {
					rest = append(rest, a)
				}
			}
			if without := managerArgs(t, render(t)); strings.Join(rest, " ") != strings.Join(without, " ") {
				t.Errorf("other arguments changed:\n with:    %v\n without: %v", rest, without)
			}
		})
	}

	// What the operator would refuse at start, the chart refuses before
	// anything is installed.
	const refusal = `collector.runAsUser must be a user ID from 1 to 2147483647, "none" or "auto"`
	for name, value := range map[string]string{
		"root": "0", "a negative number": "-1", "a name": "nobody", "a user and a group": "1001:1001", "a fraction": "1.5",
		"more than a user ID can be": "2147483648", "far more": "99999999999999999999",
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			if out := renderFailure(t, "--set-string", "collector.runAsUser="+value); !strings.Contains(out, refusal) {
				t.Errorf("helm said:\n%s", out)
			}
		})
	}
	for name, value := range map[string]string{
		"a fraction, as a number": `collector={"runAsUser":1.5}`, "root, as a number": `collector={"runAsUser":0}`,
		"more than a user ID can be, as a number": `collector={"runAsUser":2147483648}`,
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			if out := renderFailure(t, "--set-json", value); !strings.Contains(out, refusal) {
				t.Errorf("helm said:\n%s", out)
			}
		})
	}
}

// valuesFile writes a values file for `helm template -f`.
func valuesFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

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

// Package build tests the build tooling of the repository: the Makefile
// recipes that render, deploy and release the operator, and the invariants
// that tie the toolchain versions and CI workflows together.
//
// The Makefile tests run the real recipes in a scratch copy of the files they
// need. kubectl, the container tool, kind and (for the e2e target) go are
// replaced by small scripts that record how they were called, so no cluster,
// registry or network is involved.
package build

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const (
	testImage      = "registry.example.com/team/pgpilot-operator:v9.9.9"
	kustomization  = "config/manager/kustomization.yaml"
	e2eClusterName = "pgpilot-operator-test-e2e"
)

var repoRoot = func() string {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		panic(err)
	}
	return root
}()

var (
	kustomizeOnce sync.Once
	kustomizeErr  error
)

// kustomizeBinary returns the kustomize the Makefile installs into bin/,
// installing it first when it is not there yet.
func kustomizeBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(repoRoot, "bin", "kustomize")
	kustomizeOnce.Do(func() {
		if _, err := os.Stat(path); err == nil {
			return
		}
		out, err := exec.Command("make", "-C", repoRoot, "--no-print-directory", "kustomize").CombinedOutput()
		if err != nil {
			kustomizeErr = fmt.Errorf("make kustomize: %w\n%s", err, out)
		}
	})
	if kustomizeErr != nil {
		t.Fatal(kustomizeErr)
	}
	return path
}

// workspace is a scratch copy of the files the Makefile recipes work on,
// together with the fake tools and the directory they log into.
type workspace struct {
	dir     string // copy of Makefile, Dockerfile, go.mod and config/
	logs    string // where the fake tools record their calls
	tmp     string // TMPDIR of the recipes; must be empty again afterwards
	fakeBin string // fake kubectl, docker and kind
	goBin   string // fake go, only put on PATH by the e2e target tests
}

func newWorkspace(t *testing.T) *workspace {
	t.Helper()
	base := t.TempDir()
	w := &workspace{
		dir:     filepath.Join(base, "repo"),
		logs:    filepath.Join(base, "logs"),
		tmp:     filepath.Join(base, "tmp"),
		fakeBin: filepath.Join(base, "fakebin"),
		goBin:   filepath.Join(base, "gobin"),
	}
	for _, d := range []string{w.dir, w.logs, w.tmp, w.fakeBin, w.goBin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"Makefile", "Dockerfile", "go.mod", "go.sum"} {
		copyFile(t, filepath.Join(repoRoot, f), filepath.Join(w.dir, f))
	}
	copyDir(t, filepath.Join(repoRoot, "config"), filepath.Join(w.dir, "config"))

	// Every fake appends "<tool> <args>" to calls.log, so tests can check the
	// order of calls across tools, and its own details to <tool>.* files.

	// kubectl: records its arguments and the manifest it was given, either on
	// stdin (-f -) or as a file (-f <path>).
	w.script(t, w.fakeBin, "kubectl", `
echo "$*" >> "$FAKE_LOGS/kubectl.args"
echo "kubectl $*" >> "$FAKE_LOGS/calls.log"
file=""; prev=""
for a in "$@"; do
  if [ "$prev" = "-f" ]; then file="$a"; fi
  prev="$a"
done
if [ "$file" = "-" ]; then
  cat > "$FAKE_LOGS/kubectl.input"
elif [ -n "$file" ]; then
  cat "$file" > "$FAKE_LOGS/kubectl.input"
fi
exit "${FAKE_KUBECTL_EXIT:-0}"
`)
	// docker: records its arguments and keeps the Dockerfile a build saw.
	w.script(t, w.fakeBin, "docker", `
echo "$*" >> "$FAKE_LOGS/docker.args"
echo "docker $*" >> "$FAKE_LOGS/calls.log"
if [ "${1:-} ${2:-}" = "buildx build" ]; then
  cp Dockerfile.cross "$FAKE_LOGS/Dockerfile.cross" 2>/dev/null || true
  exit "${FAKE_DOCKER_BUILD_EXIT:-0}"
fi
exit 0
`)
	// kind: records its arguments and lists the clusters the test names.
	w.script(t, w.fakeBin, "kind", `
echo "$*" >> "$FAKE_LOGS/kind.args"
echo "kind $*" >> "$FAKE_LOGS/calls.log"
if [ "${1:-} ${2:-}" = "get clusters" ] && [ -n "${FAKE_KIND_CLUSTERS:-}" ]; then
  printf '%s\n' "$FAKE_KIND_CLUSTERS"
fi
if [ "${1:-} ${2:-}" = "delete cluster" ]; then
  exit "${FAKE_KIND_DELETE_EXIT:-0}"
fi
exit 0
`)
	// helm: records its arguments and renders one object that carries them.
	w.script(t, w.fakeBin, "helm", `
echo "$*" >> "$FAKE_LOGS/helm.args"
echo "helm $*" >> "$FAKE_LOGS/calls.log"
if [ -n "${FAKE_HELM_FAIL_ON:-}" ] && [[ "$*" == *"$FAKE_HELM_FAIL_ON"* ]]; then
  echo "Error: render failed" >&2
  exit 1
fi
printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: rendered\n  annotations:\n    helm-args: "%s"\n' "$*"
`)
	// kubeconform: records its arguments and what it was asked to validate.
	w.script(t, w.fakeBin, "kubeconform", `
echo "$*" >> "$FAKE_LOGS/kubeconform.args"
echo "kubeconform $*" >> "$FAKE_LOGS/calls.log"
input="$(cat)"
printf '%s\n---\n' "$input" >> "$FAKE_LOGS/kubeconform.input"
if [ -n "${FAKE_KUBECONFORM_FAIL_ON:-}" ] && [[ "$input" == *"$FAKE_KUBECONFORM_FAIL_ON"* ]]; then
  echo "rendered - ConfigMap is invalid" >&2
  exit 1
fi
exit 0
`)
	// govulncheck: records its arguments and the Go environment it would scan with.
	w.script(t, w.fakeBin, "govulncheck", `
echo "$*" >> "$FAKE_LOGS/govulncheck.args"
echo "govulncheck $*" >> "$FAKE_LOGS/calls.log"
{
  echo "GOROOT=${GOROOT:-<unset>}"
  echo "PATH_FIRST=${PATH%%:*}"
} >> "$FAKE_LOGS/govulncheck.env"
exit "${FAKE_GOVULNCHECK_EXIT:-0}"
`)
	// go: records each call with the environment that matters to the recipes.
	// "env GOROOT" answers with $FAKE_GOROOT, "install" creates the binary a
	// real install would, "test" exits with $FAKE_GO_EXIT.
	w.script(t, w.goBin, "go", `
echo "$*" >> "$FAKE_LOGS/go.args"
echo "go $*" >> "$FAKE_LOGS/calls.log"
{
  echo "--- go $*"
  echo "KUBECONFIG=${KUBECONFIG:-}"
  echo "CERT_MANAGER_INSTALL_SKIP=${CERT_MANAGER_INSTALL_SKIP:-}"
  echo "KIND=${KIND:-}"
  echo "KIND_CLUSTER=${KIND_CLUSTER:-}"
  echo "GOTOOLCHAIN=${GOTOOLCHAIN:-}"
  echo "GOROOT=${GOROOT:-<unset>}"
  echo "GOBIN=${GOBIN:-}"
} >> "$FAKE_LOGS/go.env"
case "${1:-}" in
  env)
    if [ "${2:-}" = "GOROOT" ]; then echo "${FAKE_GOROOT:-}"; fi
    ;;
  install)
    name="$(basename "${2%@*}")"
    : > "$GOBIN/$name"
    chmod +x "$GOBIN/$name"
    ;;
  test)
    exit "${FAKE_GO_EXIT:-0}"
    ;;
esac
exit 0
`)
	return w
}

func (w *workspace) script(t *testing.T, dir, name, body string) {
	t.Helper()
	content := "#!/usr/bin/env bash\nset -u\n" + strings.TrimLeft(body, "\n")
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

// inheritedEnv lists the variables of the test process that make may see.
var inheritedEnv = []string{
	"HOME", "USER", "LANG",
	"GOROOT", "GOPATH", "GOCACHE", "GOMODCACHE", "GOPROXY", "GOFLAGS", "GOTOOLCHAIN",
}

type makeRun struct {
	env    []string // extra environment, KEY=value
	fakeGo bool     // put the fake go first on PATH
}

// make runs a target of the workspace Makefile. Code generation and the Go
// tooling are marked as up to date: these tests are about the recipes that
// follow them.
func (w *workspace) make(t *testing.T, run makeRun, args ...string) (string, error) {
	t.Helper()
	base := []string{
		"-C", w.dir, "--no-print-directory",
		"-o", "manifests", "-o", "generate", "-o", "fmt", "-o", "vet",
		// kustomize comes from the repository's bin/; its install rule must
		// not run from here.
		"-o", "kustomize",
		"KUSTOMIZE=" + kustomizeBinary(t),
		"KUBECTL=" + filepath.Join(w.fakeBin, "kubectl"),
		"CONTAINER_TOOL=" + filepath.Join(w.fakeBin, "docker"),
		"KIND=" + filepath.Join(w.fakeBin, "kind"),
		"LOCALBIN=" + filepath.Join(w.dir, "bin"),
	}
	full := make([]string, 0, len(base)+len(args))
	full = append(append(full, base...), args...)

	path := os.Getenv("PATH")
	if run.fakeGo {
		path = w.goBin + string(os.PathListSeparator) + path
	}
	cmd := exec.Command("make", full...)
	// The recipes must behave the same on every machine: only what a build
	// needs is inherited. Anything else - in particular the variables the
	// Makefile reads with ?=, such as KIND_CLUSTER, E2E_KUBECONFIG or
	// PLATFORMS, and a parent make's MAKEFLAGS - stays out.
	cmd.Env = []string{
		"PATH=" + path,
		"TMPDIR=" + w.tmp,
		"FAKE_LOGS=" + w.logs,
	}
	for _, key := range inheritedEnv {
		if v, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+v)
		}
	}
	cmd.Env = append(cmd.Env, run.env...)

	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

func (w *workspace) read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(w.dir, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// log returns what a fake tool recorded, or "" when it was never called.
func (w *workspace) log(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(w.logs, name))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (w *workspace) calls(t *testing.T, name string) []string {
	t.Helper()
	text := strings.TrimSpace(w.log(t, name))
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func (w *workspace) exists(rel string) bool {
	_, err := os.Stat(filepath.Join(w.dir, rel))
	return err == nil
}

// expectNoTempLeft fails when a recipe left files behind in its TMPDIR.
func (w *workspace) expectNoTempLeft(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(w.tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("recipe left temporary files behind: %v", names)
	}
}

// breakKustomize makes `kustomize build` fail for the given config directory.
func (w *workspace) breakKustomize(t *testing.T, configDir string) {
	t.Helper()
	path := filepath.Join(w.dir, "config", configDir, "kustomization.yaml")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString("\nresources:\n- this-file-does-not-exist.yaml\n"); err != nil {
		t.Fatal(err)
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		copyFile(t, path, filepath.Join(dst, rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func expectSuccess(t *testing.T, out string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("make failed: %v\n%s", err, out)
	}
}

func expectFailure(t *testing.T, out string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("make succeeded, want a failure\n%s", out)
	}
}

func expectManagerImage(t *testing.T, manifest, image string) {
	t.Helper()
	if !strings.Contains(manifest, "image: "+image+"\n") {
		t.Errorf("rendered manifest does not run the manager as %s", image)
	}
	if strings.Contains(manifest, "image: controller:latest") {
		t.Error("rendered manifest still contains the placeholder image controller:latest")
	}
	for _, kind := range []string{"kind: CustomResourceDefinition", "kind: Deployment", "kind: ClusterRole"} {
		if !strings.Contains(manifest, kind) {
			t.Errorf("rendered manifest contains no %q", kind)
		}
	}
}

// ---------------------------------------------------------------------------
// build-installer
// ---------------------------------------------------------------------------

// The recipe used to run `kustomize edit set image` in config/manager, which
// rewrote a tracked file on every build and deploy.
func TestBuildInstaller_RendersImageWithoutTouchingTrackedFiles(t *testing.T) {
	w := newWorkspace(t)
	before := w.read(t, kustomization)

	out, err := w.make(t, makeRun{}, "build-installer", "IMG="+testImage)
	expectSuccess(t, out, err)

	expectManagerImage(t, w.read(t, "dist/install.yaml"), testImage)
	if after := w.read(t, kustomization); after != before {
		t.Errorf("%s was modified by build-installer:\n%s", kustomization, after)
	}
	w.expectNoTempLeft(t)
}

func TestBuildInstaller_FailsWhenRenderingFails(t *testing.T) {
	w := newWorkspace(t)
	w.breakKustomize(t, "default")

	out, err := w.make(t, makeRun{}, "build-installer", "IMG="+testImage)
	expectFailure(t, out, err)

	if w.exists("dist/install.yaml") {
		t.Errorf("a failed build left dist/install.yaml behind:\n%s", w.read(t, "dist/install.yaml"))
	}
	w.expectNoTempLeft(t)
}

// A previous good install.yaml must survive a failed rebuild untouched.
func TestBuildInstaller_KeepsPreviousOutputOnFailure(t *testing.T) {
	w := newWorkspace(t)
	out, err := w.make(t, makeRun{}, "build-installer", "IMG="+testImage)
	expectSuccess(t, out, err)
	good := w.read(t, "dist/install.yaml")

	w.breakKustomize(t, "default")
	out, err = w.make(t, makeRun{}, "build-installer", "IMG=registry.example.com/other:v1")
	expectFailure(t, out, err)

	if got := w.read(t, "dist/install.yaml"); got != good {
		t.Error("a failed build overwrote the previous dist/install.yaml")
	}
}

// ---------------------------------------------------------------------------
// deploy / undeploy
// ---------------------------------------------------------------------------

func TestDeploy_AppliesRenderedManifests(t *testing.T) {
	w := newWorkspace(t)
	before := w.read(t, kustomization)

	out, err := w.make(t, makeRun{}, "deploy", "IMG="+testImage)
	expectSuccess(t, out, err)

	calls := w.calls(t, "kubectl.args")
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "apply -f ") {
		t.Fatalf("kubectl calls = %q, want exactly one `apply -f <file>`", calls)
	}
	expectManagerImage(t, w.log(t, "kubectl.input"), testImage)
	if after := w.read(t, kustomization); after != before {
		t.Errorf("%s was modified by deploy", kustomization)
	}
	w.expectNoTempLeft(t)
}

func TestDeploy_DoesNotCallKubectlWhenRenderingFails(t *testing.T) {
	w := newWorkspace(t)
	w.breakKustomize(t, "default")

	out, err := w.make(t, makeRun{}, "deploy", "IMG="+testImage)
	expectFailure(t, out, err)

	if calls := w.calls(t, "kubectl.args"); len(calls) != 0 {
		t.Errorf("kubectl was called although rendering failed: %q", calls)
	}
	w.expectNoTempLeft(t)
}

func TestDeploy_FailsWhenKubectlFails(t *testing.T) {
	w := newWorkspace(t)

	out, err := w.make(t, makeRun{env: []string{"FAKE_KUBECTL_EXIT=1"}}, "deploy", "IMG="+testImage)
	expectFailure(t, out, err)
	w.expectNoTempLeft(t)
}

func TestUndeploy(t *testing.T) {
	t.Run("deletes the rendered manifests", func(t *testing.T) {
		w := newWorkspace(t)
		out, err := w.make(t, makeRun{}, "undeploy")
		expectSuccess(t, out, err)

		calls := w.calls(t, "kubectl.args")
		if len(calls) != 1 || calls[0] != "delete --ignore-not-found=false -f -" {
			t.Fatalf("kubectl calls = %q", calls)
		}
		if !strings.Contains(w.log(t, "kubectl.input"), "kind: Deployment") {
			t.Error("undeploy passed no Deployment to kubectl")
		}
	})

	t.Run("passes ignore-not-found through", func(t *testing.T) {
		w := newWorkspace(t)
		out, err := w.make(t, makeRun{}, "undeploy", "ignore-not-found=true")
		expectSuccess(t, out, err)
		if calls := w.calls(t, "kubectl.args"); len(calls) != 1 || calls[0] != "delete --ignore-not-found=true -f -" {
			t.Fatalf("kubectl calls = %q", calls)
		}
	})

	t.Run("does not call kubectl when rendering fails", func(t *testing.T) {
		w := newWorkspace(t)
		w.breakKustomize(t, "default")
		out, err := w.make(t, makeRun{}, "undeploy")
		expectFailure(t, out, err)
		if calls := w.calls(t, "kubectl.args"); len(calls) != 0 {
			t.Errorf("kubectl was called although rendering failed: %q", calls)
		}
	})

	t.Run("fails when kubectl fails", func(t *testing.T) {
		w := newWorkspace(t)
		out, err := w.make(t, makeRun{env: []string{"FAKE_KUBECTL_EXIT=1"}}, "undeploy")
		expectFailure(t, out, err)
	})
}

// ---------------------------------------------------------------------------
// install / uninstall
// ---------------------------------------------------------------------------

// The recipes used to discard kustomize's errors and exit status: a broken
// config/crd printed "No CRDs to install" and reported success.
func TestInstallAndUninstall(t *testing.T) {
	targets := map[string]string{
		"install":   "apply -f -",
		"uninstall": "delete --ignore-not-found=false -f -",
	}
	for target, wantCall := range targets {
		t.Run(target+" passes the CRDs to kubectl", func(t *testing.T) {
			w := newWorkspace(t)
			out, err := w.make(t, makeRun{}, target)
			expectSuccess(t, out, err)

			if calls := w.calls(t, "kubectl.args"); len(calls) != 1 || calls[0] != wantCall {
				t.Fatalf("kubectl calls = %q, want [%q]", calls, wantCall)
			}
			input := w.log(t, "kubectl.input")
			for _, crd := range []string{"pgpilotmonitors.pgpilot.io", "pgpilotmetriclibraries.pgpilot.io"} {
				if !strings.Contains(input, "name: "+crd) {
					t.Errorf("%s did not pass CRD %s to kubectl", target, crd)
				}
			}
			if strings.Contains(input, "kind: Deployment") {
				t.Errorf("%s must only handle CRDs, but passed a Deployment", target)
			}
		})

		t.Run(target+" fails when kustomize fails", func(t *testing.T) {
			w := newWorkspace(t)
			w.breakKustomize(t, "crd")
			out, err := w.make(t, makeRun{}, target)
			expectFailure(t, out, err)
			if calls := w.calls(t, "kubectl.args"); len(calls) != 0 {
				t.Errorf("kubectl was called although kustomize failed: %q", calls)
			}
		})

		t.Run(target+" fails when kubectl fails", func(t *testing.T) {
			w := newWorkspace(t)
			out, err := w.make(t, makeRun{env: []string{"FAKE_KUBECTL_EXIT=1"}}, target)
			expectFailure(t, out, err)
		})
	}

	t.Run("uninstall passes ignore-not-found through", func(t *testing.T) {
		w := newWorkspace(t)
		out, err := w.make(t, makeRun{}, "uninstall", "ignore-not-found=true")
		expectSuccess(t, out, err)
		if calls := w.calls(t, "kubectl.args"); len(calls) != 1 || calls[0] != "delete --ignore-not-found=true -f -" {
			t.Fatalf("kubectl calls = %q", calls)
		}
	})
}

// ---------------------------------------------------------------------------
// docker-buildx
// ---------------------------------------------------------------------------

func TestDockerBuildx_BuildsForBothArchitecturesAndCleansUp(t *testing.T) {
	w := newWorkspace(t)

	out, err := w.make(t, makeRun{}, "docker-buildx", "IMG="+testImage)
	expectSuccess(t, out, err)

	calls := w.calls(t, "docker.args")
	want := []string{
		"buildx create --name pgpilot-operator-builder",
		"buildx use pgpilot-operator-builder",
		"buildx build --push --platform=linux/arm64,linux/amd64 --tag " + testImage + " -f Dockerfile.cross .",
		"buildx rm pgpilot-operator-builder",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("docker calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
	if w.exists("Dockerfile.cross") {
		t.Error("Dockerfile.cross was not removed")
	}

	// The cross Dockerfile builds on the build platform and keeps the pins.
	cross := w.log(t, "Dockerfile.cross")
	var from []string
	for line := range strings.SplitSeq(cross, "\n") {
		if strings.HasPrefix(line, "FROM ") {
			from = append(from, line)
		}
	}
	if len(from) != 2 {
		t.Fatalf("Dockerfile.cross has %d FROM lines, want 2:\n%s", len(from), cross)
	}
	if !strings.HasPrefix(from[0], "FROM --platform=${BUILDPLATFORM} golang:") || !strings.Contains(from[0], "@sha256:") {
		t.Errorf("builder stage = %q, want --platform=${BUILDPLATFORM} and the pinned golang image", from[0])
	}
	if strings.Contains(from[1], "--platform") {
		t.Errorf("the final stage must be built for the target platform, got %q", from[1])
	}
}

// The build line used to be prefixed with "-": a failed multi-arch build or
// push printed "Error 1 (ignored)" and the target reported success.
func TestDockerBuildx_FailsWhenTheBuildFailsButStillCleansUp(t *testing.T) {
	w := newWorkspace(t)

	out, err := w.make(t, makeRun{env: []string{"FAKE_DOCKER_BUILD_EXIT=7"}}, "docker-buildx", "IMG="+testImage)
	expectFailure(t, out, err)

	calls := w.calls(t, "docker.args")
	if len(calls) == 0 || calls[len(calls)-1] != "buildx rm pgpilot-operator-builder" {
		t.Errorf("the builder was not removed after a failed build; docker calls: %q", calls)
	}
	if w.exists("Dockerfile.cross") {
		t.Error("Dockerfile.cross was not removed after a failed build")
	}
}

// ---------------------------------------------------------------------------
// e2e targets
// ---------------------------------------------------------------------------

func e2eKubeconfig(w *workspace) string {
	return filepath.Join(w.dir, "bin", "e2e-kubeconfig")
}

func TestSetupTestE2E(t *testing.T) {
	t.Run("creates the cluster with a kubeconfig of its own", func(t *testing.T) {
		w := newWorkspace(t)
		out, err := w.make(t, makeRun{}, "setup-test-e2e")
		expectSuccess(t, out, err)

		want := []string{
			"get clusters",
			"create cluster --name " + e2eClusterName + " --kubeconfig " + e2eKubeconfig(w),
			"export kubeconfig --name " + e2eClusterName + " --kubeconfig " + e2eKubeconfig(w),
		}
		if got := w.calls(t, "kind.args"); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("kind calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	})

	t.Run("reuses an existing cluster but still exports its kubeconfig", func(t *testing.T) {
		w := newWorkspace(t)
		out, err := w.make(t, makeRun{env: []string{"FAKE_KIND_CLUSTERS=other\n" + e2eClusterName}}, "setup-test-e2e")
		expectSuccess(t, out, err)

		want := []string{
			"get clusters",
			"export kubeconfig --name " + e2eClusterName + " --kubeconfig " + e2eKubeconfig(w),
		}
		if got := w.calls(t, "kind.args"); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("kind calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	})

	// The old check was a substring match, so "<name>-old" counted as the
	// cluster and the suite went on without creating its own.
	for _, similar := range []string{e2eClusterName + "-old", "my-" + e2eClusterName, "pgpilot-operator-test"} {
		t.Run("creates the cluster when only "+similar+" exists", func(t *testing.T) {
			w := newWorkspace(t)
			out, err := w.make(t, makeRun{env: []string{"FAKE_KIND_CLUSTERS=" + similar}}, "setup-test-e2e")
			expectSuccess(t, out, err)

			created := false
			for _, call := range w.calls(t, "kind.args") {
				if strings.HasPrefix(call, "create cluster --name "+e2eClusterName+" ") {
					created = true
				}
			}
			if !created {
				t.Errorf("no cluster was created; kind calls: %q", w.calls(t, "kind.args"))
			}
		})
	}

	t.Run("honours KIND_CLUSTER", func(t *testing.T) {
		w := newWorkspace(t)
		out, err := w.make(t, makeRun{}, "setup-test-e2e", "KIND_CLUSTER=custom")
		expectSuccess(t, out, err)
		if got := w.calls(t, "kind.args"); len(got) != 3 || !strings.HasPrefix(got[1], "create cluster --name custom ") {
			t.Errorf("kind calls = %q", got)
		}
	})
}

// Without a kubeconfig of its own the suite ran `make install`, `make deploy`
// and then `make undeploy` / `make uninstall` against whatever context was
// current on the developer's machine.
func TestTestE2E_RunsTheSuiteAgainstItsOwnKubeconfig(t *testing.T) {
	w := newWorkspace(t)

	out, err := w.make(t, makeRun{fakeGo: true}, "test-e2e")
	expectSuccess(t, out, err)

	// The Makefile itself asks `go env` for a few settings; only the test
	// invocations matter here.
	var tests []string
	for _, call := range w.calls(t, "go.args") {
		if strings.HasPrefix(call, "test ") {
			tests = append(tests, call)
		}
	}
	if len(tests) != 1 || tests[0] != "test -tags=e2e ./test/e2e/ -v -ginkgo.v" {
		t.Fatalf("go test calls = %q", tests)
	}
	env := w.log(t, "go.env")
	for _, want := range []string{
		"KUBECONFIG=" + e2eKubeconfig(w),
		"CERT_MANAGER_INSTALL_SKIP=true",
		"KIND=" + filepath.Join(w.fakeBin, "kind"),
		"KIND_CLUSTER=" + e2eClusterName,
	} {
		if !strings.Contains(env, want+"\n") {
			t.Errorf("the suite did not run with %s; environment was:\n%s", want, env)
		}
	}

	kind := w.calls(t, "kind.args")
	wantLast := "delete cluster --name " + e2eClusterName + " --kubeconfig " + e2eKubeconfig(w)
	if len(kind) == 0 || kind[len(kind)-1] != wantLast {
		t.Errorf("the cluster was not deleted afterwards; kind calls: %q", kind)
	}
}

func TestTestE2E_CertManagerCanBeRequested(t *testing.T) {
	w := newWorkspace(t)

	out, err := w.make(t, makeRun{fakeGo: true}, "test-e2e", "CERT_MANAGER_INSTALL_SKIP=false")
	expectSuccess(t, out, err)
	if env := w.log(t, "go.env"); !strings.Contains(env, "CERT_MANAGER_INSTALL_SKIP=false\n") {
		t.Errorf("CERT_MANAGER_INSTALL_SKIP=false did not reach the suite:\n%s", env)
	}
}

// A failing suite used to skip the cleanup line, leaving the cluster behind
// for the next run to pick up.
func TestTestE2E_FailsWithTheSuiteAndStillDeletesTheCluster(t *testing.T) {
	w := newWorkspace(t)

	out, err := w.make(t, makeRun{fakeGo: true, env: []string{"FAKE_GO_EXIT=1"}}, "test-e2e")
	expectFailure(t, out, err)

	kind := w.calls(t, "kind.args")
	wantLast := "delete cluster --name " + e2eClusterName + " --kubeconfig " + e2eKubeconfig(w)
	if len(kind) == 0 || kind[len(kind)-1] != wantLast {
		t.Errorf("the cluster was not deleted after a failed suite; kind calls: %q", kind)
	}
}

func TestCleanupTestE2E_RemovesItsOwnKubeconfig(t *testing.T) {
	w := newWorkspace(t)
	kubeconfig := e2eKubeconfig(w)
	if err := os.MkdirAll(filepath.Dir(kubeconfig), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kubeconfig, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := w.make(t, makeRun{}, "cleanup-test-e2e")
	expectSuccess(t, out, err)

	if _, err := os.Stat(kubeconfig); !os.IsNotExist(err) {
		t.Errorf("the e2e kubeconfig still exists after cleanup (stat error: %v)", err)
	}
}

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
	"strings"
	"testing"
)

const kubeconformArgs = "-strict -summary -kubernetes-version 1.31.0 -skip CustomResourceDefinition -"

// goEnvFor returns the environment the fake go recorded for the first call
// whose arguments start with the given prefix.
func goEnvFor(t *testing.T, w *workspace, callPrefix string) map[string]string {
	t.Helper()
	for block := range strings.SplitSeq(w.log(t, "go.env"), "--- go ") {
		header, rest, _ := strings.Cut(block, "\n")
		if !strings.HasPrefix(header, callPrefix) {
			continue
		}
		env := map[string]string{}
		for line := range strings.SplitSeq(strings.TrimSpace(rest), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				env[k] = v
			}
		}
		return env
	}
	t.Fatalf("go was never called with %q; calls: %q", callPrefix, w.calls(t, "go.args"))
	return nil
}

func wantToolchain(t *testing.T) string {
	t.Helper()
	return "go" + goModVersion(t) + "+auto"
}

// ---------------------------------------------------------------------------
// helm-validate
// ---------------------------------------------------------------------------

func helmValidate(t *testing.T, w *workspace, run makeRun, args ...string) (string, error) {
	t.Helper()
	full := make([]string, 0, 5+len(args))
	full = append(full,
		"helm-validate", "-o", "kubeconform",
		"HELM="+filepath.Join(w.fakeBin, "helm"),
		"KUBECONFORM="+filepath.Join(w.fakeBin, "kubeconform"),
	)
	return w.make(t, run, append(full, args...)...)
}

func TestHelmValidate_RendersAndValidatesEveryValueSet(t *testing.T) {
	w := newWorkspace(t)

	out, err := helmValidate(t, w, makeRun{}, "HELM_VALIDATE_SETS=- a=1 b=2,c=3")
	expectSuccess(t, out, err)

	const template = "template pgpilot-operator charts/pgpilot-operator --namespace pgpilot-system --kube-version 1.31.0"
	wantHelm := []string{
		template,
		template + " --set a=1",
		template + " --set b=2,c=3",
	}
	if got := w.calls(t, "helm.args"); strings.Join(got, "\n") != strings.Join(wantHelm, "\n") {
		t.Errorf("helm calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(wantHelm, "\n"))
	}

	conform := w.calls(t, "kubeconform.args")
	if len(conform) != 3 {
		t.Fatalf("kubeconform ran %d times, want once per value set (3)", len(conform))
	}
	for _, call := range conform {
		if call != kubeconformArgs {
			t.Errorf("kubeconform args = %q, want %q", call, kubeconformArgs)
		}
	}

	// Each render has to reach the validator, not just be produced.
	validated := w.log(t, "kubeconform.input")
	for _, call := range wantHelm {
		if !strings.Contains(validated, `helm-args: "`+call+`"`) {
			t.Errorf("the output of `helm %s` was not validated", call)
		}
	}

	// Rendering and validation alternate: a set is checked before the next.
	calls := w.calls(t, "calls.log")
	order := make([]string, 0, len(calls))
	for _, call := range calls {
		order = append(order, strings.Fields(call)[0])
	}
	if got := strings.Join(order, " "); got != "helm kubeconform helm kubeconform helm kubeconform" {
		t.Errorf("call order = %q", got)
	}
}

// The default list is what CI runs; it has to keep covering every toggle that
// adds or removes objects.
func TestHelmValidate_DefaultSetsCoverTheChartToggles(t *testing.T) {
	w := newWorkspace(t)

	out, err := helmValidate(t, w, makeRun{})
	expectSuccess(t, out, err)

	calls := w.calls(t, "helm.args")
	if len(calls) == 0 || strings.Contains(calls[0], "--set") {
		t.Fatalf("the first render must use the chart defaults; helm calls: %q", calls)
	}
	all := strings.Join(calls, "\n")
	for _, toggle := range []string{
		"--set installCRDs=false",
		"--set rbac.create=false",
		"--set leaderElection.enabled=false",
		"--set metrics.enabled=false",
		"--set metrics.secure=false",
		"--set serviceAccount.create=false,serviceAccount.name=existing",
	} {
		if !strings.Contains(all+"\n", toggle+"\n") {
			t.Errorf("helm-validate does not render with %q", toggle)
		}
	}
	if got := len(w.calls(t, "kubeconform.args")); got != len(calls) {
		t.Errorf("%d renders but %d validations", len(calls), got)
	}
}

func TestHelmValidate_FailsWhenARenderFails(t *testing.T) {
	w := newWorkspace(t)

	out, err := helmValidate(t, w, makeRun{env: []string{"FAKE_HELM_FAIL_ON=--set a=1"}},
		"HELM_VALIDATE_SETS=- a=1 b=2")
	expectFailure(t, out, err)

	if got := len(w.calls(t, "helm.args")); got != 2 {
		t.Errorf("helm ran %d times, want it to stop at the failing set (2)", got)
	}
	if got := len(w.calls(t, "kubeconform.args")); got != 1 {
		t.Errorf("kubeconform ran %d times, want 1: a failed render must not be validated", got)
	}
}

func TestHelmValidate_FailsWhenAnObjectIsInvalid(t *testing.T) {
	w := newWorkspace(t)

	out, err := helmValidate(t, w, makeRun{env: []string{"FAKE_KUBECONFORM_FAIL_ON=--set a=1"}},
		"HELM_VALIDATE_SETS=- a=1 b=2")
	expectFailure(t, out, err)

	if got := len(w.calls(t, "helm.args")); got != 2 {
		t.Errorf("helm ran %d times, want it to stop after the invalid set (2)", got)
	}
}

// The last set failing must fail the target too: the loop's own status is
// that of its final command only by accident.
func TestHelmValidate_FailsWhenTheLastSetIsInvalid(t *testing.T) {
	w := newWorkspace(t)

	out, err := helmValidate(t, w, makeRun{env: []string{"FAKE_KUBECONFORM_FAIL_ON=--set b=2"}},
		"HELM_VALIDATE_SETS=- a=1 b=2")
	expectFailure(t, out, err)
}

func TestHelmValidate_FailsWithoutHelm(t *testing.T) {
	w := newWorkspace(t)

	out, err := w.make(t, makeRun{}, "helm-validate", "-o", "kubeconform",
		"HELM="+filepath.Join(w.fakeBin, "no-such-helm"),
		"KUBECONFORM="+filepath.Join(w.fakeBin, "kubeconform"))
	expectFailure(t, out, err)

	if !strings.Contains(out, "helm is not installed") {
		t.Errorf("output does not explain the failure:\n%s", out)
	}
	if calls := w.calls(t, "kubeconform.args"); len(calls) != 0 {
		t.Errorf("kubeconform ran without helm: %q", calls)
	}
}

// ---------------------------------------------------------------------------
// govulncheck
// ---------------------------------------------------------------------------

func govulncheck(t *testing.T, w *workspace, extraEnv ...string) (string, error) {
	t.Helper()
	goroot := filepath.Join(w.dir, "toolchain")
	env := append([]string{
		"FAKE_GOROOT=" + goroot,
		// What a version manager exports for the locally installed Go.
		"GOROOT=/opt/old-go",
	}, extraEnv...)
	return w.make(t, makeRun{fakeGo: true, env: env},
		"govulncheck", "-o", "govulncheck-bin",
		"GOVULNCHECK="+filepath.Join(w.fakeBin, "govulncheck"))
}

// The scan has to run with the standard library of go.mod's toolchain. With
// an older Go installed and GOROOT exported it used to load the wrong one and
// fail before reporting anything.
func TestGovulncheck_ScansWithTheToolchainOfGoMod(t *testing.T) {
	w := newWorkspace(t)

	out, err := govulncheck(t, w)
	expectSuccess(t, out, err)

	if got := w.calls(t, "govulncheck.args"); len(got) != 1 || got[0] != "./..." {
		t.Fatalf("govulncheck calls = %q, want one scan of ./...", got)
	}

	lookup := goEnvFor(t, w, "env GOROOT")
	if lookup["GOTOOLCHAIN"] != wantToolchain(t) {
		t.Errorf("GOROOT was looked up with GOTOOLCHAIN=%q, want %q", lookup["GOTOOLCHAIN"], wantToolchain(t))
	}
	if lookup["GOROOT"] != "<unset>" {
		t.Errorf("GOROOT was looked up with the exported GOROOT=%q still set", lookup["GOROOT"])
	}

	scanEnv := w.log(t, "govulncheck.env")
	if !strings.Contains(scanEnv, "GOROOT=<unset>\n") {
		t.Errorf("govulncheck ran with an exported GOROOT:\n%s", scanEnv)
	}
	wantFirst := "PATH_FIRST=" + filepath.Join(w.dir, "toolchain", "bin")
	if !strings.Contains(scanEnv, wantFirst+"\n") {
		t.Errorf("the toolchain of go.mod is not first on PATH, want %s:\n%s", wantFirst, scanEnv)
	}
}

func TestGovulncheck_FailsWhenAVulnerabilityIsReachable(t *testing.T) {
	w := newWorkspace(t)

	// govulncheck exits 3 when the code calls a vulnerable symbol.
	out, err := govulncheck(t, w, "FAKE_GOVULNCHECK_EXIT=3")
	expectFailure(t, out, err)
}

// ---------------------------------------------------------------------------
// tool installation
// ---------------------------------------------------------------------------

// A linter or govulncheck built with the locally installed (older) Go cannot
// read code written for the Go of go.mod.
func TestGoInstallTool_BuildsToolsWithTheToolchainOfGoMod(t *testing.T) {
	makefile := readRepoFile(t, "Makefile")
	version := regexp.MustCompile(`(?m)^KUBECONFORM_VERSION \?= (\S+)$`).FindStringSubmatch(makefile)
	if version == nil {
		t.Fatal("Makefile does not pin KUBECONFORM_VERSION")
	}

	w := newWorkspace(t)
	bin := filepath.Join(w.dir, "bin")
	out, err := w.make(t, makeRun{fakeGo: true}, "kubeconform", "KUBECONFORM="+filepath.Join(bin, "kubeconform"))
	expectSuccess(t, out, err)

	pkg := "github.com/yannh/kubeconform/cmd/kubeconform@" + version[1]
	env := goEnvFor(t, w, "install "+pkg)
	if env["GOTOOLCHAIN"] != wantToolchain(t) {
		t.Errorf("tool was built with GOTOOLCHAIN=%q, want %q", env["GOTOOLCHAIN"], wantToolchain(t))
	}
	if env["GOBIN"] != bin {
		t.Errorf("tool was installed into %q, want %q", env["GOBIN"], bin)
	}

	// The binary is kept under a versioned name with a stable symlink to it.
	target, err := os.Readlink(filepath.Join(bin, "kubeconform"))
	if err != nil {
		t.Fatalf("bin/kubeconform is not a symlink: %v", err)
	}
	if filepath.Base(target) != "kubeconform-"+version[1] {
		t.Errorf("bin/kubeconform points at %q, want kubeconform-%s", target, version[1])
	}

	// A second run finds the tool in place and does not install again.
	out, err = w.make(t, makeRun{fakeGo: true}, "kubeconform", "KUBECONFORM="+filepath.Join(bin, "kubeconform"))
	expectSuccess(t, out, err)
	installs := 0
	for _, call := range w.calls(t, "go.args") {
		if strings.HasPrefix(call, "install ") {
			installs++
		}
	}
	if installs != 1 {
		t.Errorf("the tool was installed %d times over two runs, want 1", installs)
	}
}

// ---------------------------------------------------------------------------
// e2e targets: ordering, dry run, failure handling
// ---------------------------------------------------------------------------

// e2eSteps returns the kind calls and the go test call of a run, in order.
func e2eSteps(t *testing.T, w *workspace) []string {
	t.Helper()
	calls := w.calls(t, "calls.log")
	steps := make([]string, 0, len(calls))
	for _, call := range calls {
		if strings.HasPrefix(call, "kind ") || strings.HasPrefix(call, "go test ") {
			steps = append(steps, call)
		}
	}
	return steps
}

// The cluster has to exist, and its kubeconfig has to be written, before the
// suite starts; it is deleted only after the suite is done.
func TestTestE2E_SetsUpTheClusterBeforeTheSuiteAndDeletesItAfter(t *testing.T) {
	w := newWorkspace(t)

	out, err := w.make(t, makeRun{fakeGo: true}, "test-e2e")
	expectSuccess(t, out, err)

	kubeconfig := e2eKubeconfig(w)
	want := []string{
		"kind get clusters",
		"kind create cluster --name " + e2eClusterName + " --kubeconfig " + kubeconfig,
		"kind export kubeconfig --name " + e2eClusterName + " --kubeconfig " + kubeconfig,
		"go test -tags=e2e ./test/e2e/ -v -ginkgo.v",
		"kind delete cluster --name " + e2eClusterName + " --kubeconfig " + kubeconfig,
	}
	if got := e2eSteps(t, w); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("steps:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if _, err := os.Stat(filepath.Dir(kubeconfig)); err != nil {
		t.Errorf("the directory for the e2e kubeconfig was not created: %v", err)
	}
}

// make runs every recipe line that mentions $(MAKE) even under -n. With the
// cleanup written as a sub-make on the suite's line, a dry run started the
// real suite.
func TestTestE2E_DryRunDoesNotRunAnything(t *testing.T) {
	w := newWorkspace(t)

	out, err := w.make(t, makeRun{fakeGo: true}, "-n", "test-e2e")
	expectSuccess(t, out, err)

	if steps := e2eSteps(t, w); len(steps) != 0 {
		t.Errorf("a dry run executed: %q", steps)
	}
	if !strings.Contains(out, "go test -tags=e2e ./test/e2e/") {
		t.Errorf("the dry run does not show the suite command:\n%s", out)
	}
}

// A cluster that cannot be deleted must not turn a passing suite red; the
// next run reuses it.
func TestTestE2E_PassingSuiteStaysGreenWhenTheClusterCannotBeDeleted(t *testing.T) {
	w := newWorkspace(t)

	out, err := w.make(t, makeRun{fakeGo: true, env: []string{"FAKE_KIND_DELETE_EXIT=1"}}, "test-e2e")
	expectSuccess(t, out, err)
}

func TestTestE2E_FailingSuiteIsReportedEvenWhenTheClusterCannotBeDeleted(t *testing.T) {
	w := newWorkspace(t)

	out, err := w.make(t,
		makeRun{fakeGo: true, env: []string{"FAKE_GO_EXIT=1", "FAKE_KIND_DELETE_EXIT=1"}}, "test-e2e")
	expectFailure(t, out, err)
}

func TestSetupTestE2E_FailsWithoutKind(t *testing.T) {
	w := newWorkspace(t)

	out, err := w.make(t, makeRun{}, "setup-test-e2e", "KIND="+filepath.Join(w.fakeBin, "no-such-kind"))
	expectFailure(t, out, err)
	if !strings.Contains(out, "Kind is not installed") {
		t.Errorf("output does not explain the failure:\n%s", out)
	}
}

func TestTestE2E_DoesNotRunTheSuiteWithoutKind(t *testing.T) {
	w := newWorkspace(t)

	out, err := w.make(t, makeRun{fakeGo: true}, "test-e2e", "KIND="+filepath.Join(w.fakeBin, "no-such-kind"))
	expectFailure(t, out, err)
	if steps := e2eSteps(t, w); len(steps) != 0 {
		t.Errorf("steps ran without kind: %q", steps)
	}
}

// E2E_KUBECONFIG can be pointed at any file. Cleanup used to delete whatever
// it named - with E2E_KUBECONFIG=~/.kube/config that was the user's kubeconfig.
func TestCleanupTestE2E_LeavesAKubeconfigOutsideTheToolDirAlone(t *testing.T) {
	w := newWorkspace(t)
	foreign := filepath.Join(w.logs, "users-own-kubeconfig")
	if err := os.WriteFile(foreign, []byte("contexts: [prod]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := w.make(t, makeRun{}, "cleanup-test-e2e", "E2E_KUBECONFIG="+foreign)
	expectSuccess(t, out, err)

	want := "delete cluster --name " + e2eClusterName + " --kubeconfig " + foreign
	if got := w.calls(t, "kind.args"); len(got) != 1 || got[0] != want {
		t.Errorf("kind calls = %q, want [%q]", got, want)
	}
	content, err := os.ReadFile(foreign)
	if err != nil {
		t.Fatalf("the kubeconfig outside bin/ was removed: %v", err)
	}
	if string(content) != "contexts: [prod]\n" {
		t.Errorf("the kubeconfig outside bin/ was modified: %q", content)
	}
}

func TestCleanupTestE2E_KeepsTheKubeconfigWhenTheClusterCannotBeDeleted(t *testing.T) {
	w := newWorkspace(t)
	kubeconfig := e2eKubeconfig(w)
	if err := os.MkdirAll(filepath.Dir(kubeconfig), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kubeconfig, []byte("still needed"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := w.make(t, makeRun{env: []string{"FAKE_KIND_DELETE_EXIT=1"}}, "cleanup-test-e2e")
	expectFailure(t, out, err)

	if _, err := os.Stat(kubeconfig); err != nil {
		t.Errorf("the kubeconfig of a cluster that still exists was removed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// environment isolation
// ---------------------------------------------------------------------------

// The recipes read several variables with ?=. A value exported in the shell of
// whoever runs the tests must not change what the tests exercise - or, worse,
// what the recipes delete.
func TestMakeRuns_IgnoreVariablesExportedInTheCallersShell(t *testing.T) {
	victim := filepath.Join(t.TempDir(), "exported-kubeconfig")
	if err := os.WriteFile(victim, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("E2E_KUBECONFIG", victim)
	t.Setenv("KIND_CLUSTER", "from-the-shell")
	t.Setenv("CERT_MANAGER_INSTALL_SKIP", "false")
	t.Setenv("PLATFORMS", "linux/riscv64")
	t.Setenv("IMG", "from-the-shell:latest")
	t.Setenv("MAKEFLAGS", "-k -j8")

	w := newWorkspace(t)
	out, err := w.make(t, makeRun{fakeGo: true}, "test-e2e")
	expectSuccess(t, out, err)

	for _, call := range w.calls(t, "kind.args") {
		if strings.Contains(call, "from-the-shell") || strings.Contains(call, victim) {
			t.Errorf("an exported variable reached kind: %q", call)
		}
	}
	env := goEnvFor(t, w, "test ")
	if env["KIND_CLUSTER"] != e2eClusterName || env["CERT_MANAGER_INSTALL_SKIP"] != "true" ||
		env["KUBECONFIG"] != e2eKubeconfig(w) {
		t.Errorf("the suite ran with values from the caller's shell: %v", env)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("a file named by an exported E2E_KUBECONFIG was removed: %v", err)
	}

	out, err = w.make(t, makeRun{}, "docker-buildx", "IMG="+testImage)
	expectSuccess(t, out, err)
	build := strings.Join(w.calls(t, "docker.args"), "\n")
	if strings.Contains(build, "riscv64") || strings.Contains(build, "from-the-shell") {
		t.Errorf("an exported variable reached the image build:\n%s", build)
	}
}

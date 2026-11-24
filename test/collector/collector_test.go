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

// Package collector runs what the operator generates for a monitor against
// the real thing: the pgwatch image it would deploy, started with the
// arguments, environment, user, read-only filesystem and mounts of the
// generated Deployment, the generated ConfigMap as its configuration, and a
// PostgreSQL container to collect from.
//
// Everything else in the repository checks the generated objects against
// what we believe pgwatch does with them. These tests are where that belief
// meets the pgwatch release the operator is built for; they are the ones to
// run when that release changes (`make test-collector`).
package collector

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
	"github.com/jamalshahverdiev/pgpilot-operator/internal/builder"
)

const (
	// The role the collectors log in with; pg_monitor is what the docs tell
	// users to grant.
	monitoringUser     = "pgwatch"
	monitoringPassword = "collector-test-secret"
	credentialsSecret  = "pg-creds"

	// How long a collector gets to connect and deliver its first samples.
	firstSamples = 2 * time.Minute
)

var (
	runtime  = envOr("COLLECTOR_TEST_RUNTIME", "docker")
	postgres = envOr("COLLECTOR_TEST_POSTGRES", "postgres:18")

	// Names are unique per run, so that two runs on one machine (or what a
	// crashed run left behind) do not collide.
	runID       = randomID()
	network     = "pgpilot-collector-test-" + runID
	postgresBox = "pgpilot-collector-test-" + runID + "-postgres"
	// runLabel marks every container of this run, so that all of them can
	// be found again and removed, whatever state the tests were in.
	runLabel = "pgpilot-collector-test=" + runID

	// serverMajor is the major version of the PostgreSQL the tests run
	// against, read from the server itself.
	serverMajor int
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func randomID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// docker runs the container runtime and returns its combined output.
func docker(args ...string) (string, error) {
	var out bytes.Buffer
	cmd := exec.Command(runtime, args...)
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	if _, err := exec.LookPath(runtime); err != nil {
		fmt.Fprintf(os.Stderr, "the collector tests need a container runtime: %v\n", err)
		return 1
	}
	// In CI the version under test is the point of the job: a missing
	// variable must not quietly become the default.
	if os.Getenv("CI") != "" && os.Getenv("COLLECTOR_TEST_POSTGRES") == "" {
		fmt.Fprintln(os.Stderr, "COLLECTOR_TEST_POSTGRES is not set; run the tests through `make test-collector`")
		return 1
	}
	if out, err := docker("network", "create", "--label", runLabel, network); err != nil {
		fmt.Fprintf(os.Stderr, "create network: %v\n%s\n", err, out)
		return 1
	}
	defer removeEverything()

	// An interrupted run skips deferred calls and test cleanups alike, and
	// would leave collectors behind that keep querying the database.
	interrupted := make(chan os.Signal, 1)
	signal.Notify(interrupted, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-interrupted
		removeEverything()
		os.Exit(130)
	}()

	if out, err := docker("run", "--detach", "--name", postgresBox, "--network", network, "--label", runLabel,
		"--env", "POSTGRES_PASSWORD=superuser-of-the-test", postgres); err != nil {
		fmt.Fprintf(os.Stderr, "start %s: %v\n%s\n", postgres, err, out)
		return 1
	}
	if err := preparePostgres(); err != nil {
		logs, _ := docker("logs", "--tail", "50", postgresBox)
		fmt.Fprintf(os.Stderr, "%v\n%s\n", err, logs)
		return 1
	}
	fmt.Printf("collector tests: %s (PostgreSQL %d), pgwatch %s\n", postgres, serverMajor, builder.PgwatchVersion)
	return m.Run()
}

// removeEverything removes the containers and the network of this run.
func removeEverything() {
	if ids, err := docker("ps", "--all", "--quiet", "--filter", "label="+runLabel); err == nil && ids != "" {
		_, _ = docker(append([]string{"rm", "--force", "--volumes"}, strings.Fields(ids)...)...)
	}
	_, _ = docker("network", "rm", network)
}

// majorOfImage returns the PostgreSQL major version an image reference such
// as "postgres:18" or "postgres:18.1-alpine" promises, or 0 when the tag does
// not start with one.
func majorOfImage(image string) int {
	m := regexp.MustCompile(`:(\d+)(?:[.-][^:/]*)?$`).FindStringSubmatch(image)
	if m == nil {
		return 0
	}
	major, _ := strconv.Atoi(m[1])
	return major
}

// psql runs a statement in the PostgreSQL container, over TCP: during its
// initialisation the image runs a temporary server that listens on the Unix
// socket only, and that one must not be mistaken for the real thing.
func psql(sql string) (string, error) {
	return docker("exec", "--env", "PGPASSWORD=superuser-of-the-test", postgresBox,
		"psql", "--host", "127.0.0.1", "--username", "postgres", "--no-psqlrc", "--tuples-only", "--no-align",
		"--set", "ON_ERROR_STOP=1", "--command", sql)
}

func preparePostgres() error {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if _, err := psql("SELECT 1"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("PostgreSQL did not accept connections within two minutes")
		}
		time.Sleep(time.Second)
	}
	for _, statement := range []string{
		fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s'", monitoringUser, monitoringPassword),
		fmt.Sprintf("GRANT pg_monitor TO %s", monitoringUser),
		"CREATE DATABASE appdb",
	} {
		if out, err := psql(statement); err != nil {
			return fmt.Errorf("prepare the database (%s): %w\n%s", statement, err, out)
		}
	}

	// The job is named after a PostgreSQL version; make sure that is the
	// version the collectors really talk to.
	out, err := psql("SHOW server_version_num")
	if err != nil {
		return fmt.Errorf("read the server version: %w\n%s", err, out)
	}
	number, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return fmt.Errorf("server_version_num %q: %w", out, err)
	}
	serverMajor = number / 10000
	if want := majorOfImage(postgres); want != 0 && want != serverMajor {
		return fmt.Errorf("image %s runs PostgreSQL %d", postgres, serverMajor)
	}
	return nil
}

// newMonitor is a PgpilotMonitor for the PostgreSQL container, as a user
// would write it: only what has no default.
func newMonitor(name string, metrics pgpilotv1.MetricsSpec) *pgpilotv1.PgpilotMonitor {
	monitor := &pgpilotv1.PgpilotMonitor{}
	monitor.Name, monitor.Namespace = name, "collector-test"
	monitor.Spec = pgpilotv1.PgpilotMonitorSpec{
		Database: pgpilotv1.DatabaseRef{
			Host:              postgresBox,
			Database:          "appdb",
			CredentialsSecret: &pgpilotv1.CredentialsSecretRef{Name: credentialsSecret},
			CustomTags:        map[string]string{"env": "collector-test"},
		},
		Metrics: metrics,
		Sinks:   pgpilotv1.SinksSpec{Prometheus: &pgpilotv1.PrometheusSink{Enabled: true}},
	}
	return monitor
}

// collector is a running pgwatch container.
type collector struct {
	t       *testing.T
	name    string
	image   string
	port    int32
	monitor *pgpilotv1.PgpilotMonitor
}

// secrets is what the Secrets referenced by the generated pod would hold.
type secrets map[string]map[string]string

func defaultSecrets() secrets {
	return secrets{credentialsSecret: {"username": monitoringUser, "password": monitoringPassword}}
}

// startCollector renders the monitor with the operator's builders and starts
// the generated pod as a container. The container is described by the
// generated Deployment and nothing else: whatever the builder adds to the pod
// either translates to a flag here or fails the test, so that the two cannot
// drift apart unnoticed.
func startCollector(
	t *testing.T,
	monitor *pgpilotv1.PgpilotMonitor,
	libraries []pgpilotv1.PgpilotMetricLibrary,
	creds secrets,
) *collector {
	t.Helper()

	merged := builder.MergeMetrics(monitor.Spec.Metrics, libraries)
	configMap, hash, err := builder.BuildConfigMap(monitor, merged)
	if err != nil {
		t.Fatalf("BuildConfigMap: %v", err)
	}
	deployment := builder.BuildDeployment(monitor, merged, builder.PodTemplateVersions{Config: hash})
	pod := deployment.Spec.Template.Spec
	if len(pod.Containers) != 1 || len(pod.InitContainers) != 0 {
		t.Fatalf("the test knows a pod with one container, got %d and %d init containers",
			len(pod.Containers), len(pod.InitContainers))
	}
	container := pod.Containers[0]

	c := &collector{
		t:       t,
		name:    "pgpilot-collector-test-" + runID + "-" + monitor.Name,
		image:   container.Image,
		monitor: monitor,
	}
	c.port = probedMetricsPort(t, container)
	if len(container.Command) != 0 {
		t.Fatalf("the builder sets a command; teach the test to override the image's entrypoint")
	}
	args := slices.Concat(
		[]string{"run", "--detach", "--name", c.name, "--network", network, "--label", runLabel},
		securityFlags(t, pod, container),
		envFlags(t, container, creds),
		volumeFlags(t, pod, container, configMap),
		[]string{container.Image},
		container.Args,
	)

	out, err := docker(args...)
	if err != nil {
		t.Fatalf("start %s: %v\n%s", container.Image, err, out)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("sources.yaml:\n%s", configMap.Data[builder.SourcesKey])
			t.Logf("arguments: %q", container.Args)
			t.Logf("log of %s:\n%s", c.name, c.logs())
		}
		_, _ = docker("rm", "--force", "--volumes", c.name)
	})
	return c
}

// securityFlags translates the security settings of the pod. The kubelet
// refuses to start a container as root when the pod demands a non-root user
// and the image names none, so the numeric user is required here.
func securityFlags(t *testing.T, pod corev1.PodSpec, container corev1.Container) []string {
	t.Helper()
	if pod.SecurityContext == nil || pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot {
		t.Errorf("the pod does not demand a non-root user")
	}
	sc := container.SecurityContext
	if sc == nil || sc.RunAsUser == nil {
		t.Fatalf("the container has no numeric user: with runAsNonRoot the kubelet could not verify the image's")
	}
	user := strconv.FormatInt(*sc.RunAsUser, 10)
	if sc.RunAsGroup != nil {
		user += ":" + strconv.FormatInt(*sc.RunAsGroup, 10)
	}
	flags := []string{"--user", user}
	if sc.ReadOnlyRootFilesystem != nil && *sc.ReadOnlyRootFilesystem {
		flags = append(flags, "--read-only")
	}
	if sc.AllowPrivilegeEscalation != nil && !*sc.AllowPrivilegeEscalation {
		flags = append(flags, "--security-opt", "no-new-privileges")
	}
	if sc.Capabilities != nil {
		for _, capability := range sc.Capabilities.Drop {
			flags = append(flags, "--cap-drop", string(capability))
		}
		for _, capability := range sc.Capabilities.Add {
			flags = append(flags, "--cap-add", string(capability))
		}
	}
	return flags
}

// envFlags resolves the environment of the container, reading Secret
// references from creds.
func envFlags(t *testing.T, container corev1.Container, creds secrets) []string {
	t.Helper()
	flags := make([]string, 0, 2*len(container.Env))
	for _, env := range container.Env {
		value := env.Value
		if env.ValueFrom != nil {
			ref := env.ValueFrom.SecretKeyRef
			if ref == nil {
				t.Fatalf("env %s: the test only knows values and Secret references", env.Name)
			}
			var ok bool
			if value, ok = creds[ref.Name][ref.Key]; !ok {
				t.Fatalf("env %s refers to key %q of Secret %q, which the test does not have", env.Name, ref.Key, ref.Name)
			}
		}
		flags = append(flags, "--env", env.Name+"="+value)
	}
	return flags
}

// volumeFlags mounts the generated ConfigMap and the emptyDir volumes.
func volumeFlags(t *testing.T, pod corev1.PodSpec, container corev1.Container, configMap *corev1.ConfigMap) []string {
	t.Helper()
	volumes := map[string]corev1.Volume{}
	for _, v := range pod.Volumes {
		volumes[v.Name] = v
	}
	var flags []string
	for _, mount := range container.VolumeMounts {
		volume, ok := volumes[mount.Name]
		switch {
		case !ok:
			t.Fatalf("volumeMount %q has no volume", mount.Name)
		case volume.ConfigMap != nil:
			if volume.ConfigMap.Name != configMap.Name {
				t.Fatalf("the pod mounts ConfigMap %q, the builder made %q", volume.ConfigMap.Name, configMap.Name)
			}
			if !mount.ReadOnly {
				t.Errorf("the configuration is mounted writable")
			}
			flags = append(flags, "--volume", writeConfig(t, configMap.Data)+":"+mount.MountPath+":ro")
		case volume.EmptyDir != nil:
			flags = append(flags, "--tmpfs", mount.MountPath)
		default:
			t.Fatalf("volume %q: the test only knows ConfigMap and emptyDir volumes", mount.Name)
		}
	}
	return flags
}

// probedMetricsPort returns the port named metrics, after checking that the
// probes are the TCP checks of that port the test knows how to imitate.
func probedMetricsPort(t *testing.T, container corev1.Container) int32 {
	t.Helper()
	var metrics int32
	for _, port := range container.Ports {
		if port.Name == "metrics" {
			metrics = port.ContainerPort
		}
	}
	if metrics == 0 {
		t.Fatalf("the container has no port named metrics")
	}
	probes := map[string]*corev1.Probe{"liveness": container.LivenessProbe, "readiness": container.ReadinessProbe}
	for name, probe := range probes {
		if probe == nil || probe.TCPSocket == nil || probe.TCPSocket.Port.IntValue() != int(metrics) {
			t.Fatalf("the %s probe is not a TCP check of the metrics port; teach the test how the kubelet runs it", name)
		}
	}
	return metrics
}

// writeConfig lays the ConfigMap out the way the kubelet does: one
// world-readable file per key.
func writeConfig(t *testing.T, data map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range data {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func (c *collector) logs() string {
	out, _ := docker("logs", c.name)
	return out
}

func (c *collector) running() bool {
	out, err := docker("inspect", "--format", "{{.State.Running}} {{.State.ExitCode}}", c.name)
	return err == nil && strings.HasPrefix(out, "true")
}

// The other containers have no HTTP client, and the collector publishes no
// port to the host. Bash in the PostgreSQL container reaches it over the
// network the two share, as the kubelet and Prometheus would over the pod
// network.
const tcpConnect = `exec 3<>"/dev/tcp/$0/$1"`

// accepts reports whether the collector accepts TCP connections on a port,
// which is all its probes ask.
func (c *collector) accepts(port int32) bool {
	_, err := docker("exec", postgresBox, "timeout", "5", "bash", "-c", tcpConnect, c.name, strconv.Itoa(int(port)))
	return err == nil
}

// scrape fetches /metrics.
func (c *collector) scrape() (samples, error) {
	const get = tcpConnect + ` && printf 'GET /metrics HTTP/1.0\r\nHost: collector\r\n\r\n' >&3 && cat <&3`
	out, err := docker("exec", postgresBox, "timeout", "30", "bash", "-c", get, c.name, strconv.Itoa(int(c.port)))
	if err != nil {
		return nil, fmt.Errorf("scrape: %w: %s", err, out)
	}
	head, body, ok := strings.Cut(strings.ReplaceAll(out, "\r\n", "\n"), "\n\n")
	if !ok || !strings.HasPrefix(head, "HTTP/1.0 200") && !strings.HasPrefix(head, "HTTP/1.1 200") {
		return nil, fmt.Errorf("scrape: unexpected response: %.200s", out)
	}
	return parseSamples(body)
}

// eventually polls until check returns nil, for as long as a collector gets
// to deliver its first samples.
func (c *collector) eventually(what string, check func(samples) error) samples {
	c.t.Helper()
	deadline := time.Now().Add(firstSamples)
	var last error
	for {
		if !c.running() {
			c.t.Fatalf("%s: the collector exited", what)
		}
		got, err := c.scrape()
		if err == nil {
			if err = check(got); err == nil {
				return got
			}
		}
		last = err
		if time.Now().After(deadline) {
			c.t.Fatalf("%s: not within %s: %v", what, firstSamples, last)
		}
		time.Sleep(2 * time.Second)
	}
}

// expectCleanLog fails on the lines pgwatch writes when it cannot use what
// the operator gave it.
func (c *collector) expectCleanLog() {
	c.t.Helper()
	for line := range strings.SplitSeq(c.logs(), "\n") {
		lower := strings.ToLower(line)
		for _, bad := range []string{
			"metric definition not found",                // a source asks for a metric the registry lacks
			"no sql found",                               // a definition without SQL for this server version
			"read-only file system",                      // pgwatch wants to write where the pod cannot
			"permission denied",                          // ... or may not
			"unknown field", "cannot unmarshal", "yaml:", // configuration it cannot read
			"[error]", "level=error", "[fatal]", "panic:",
		} {
			if strings.Contains(lower, bad) {
				c.t.Errorf("unexpected line in the collector's log: %s", line)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Prometheus text format, as much of it as these tests need
// ---------------------------------------------------------------------------

type sample struct {
	name   string
	labels map[string]string
	value  float64
}

type samples []sample

func parseSamples(text string) (samples, error) {
	var out samples
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		s := sample{labels: map[string]string{}}
		var rest string
		if open := strings.IndexByte(line, '{'); open >= 0 {
			closing := strings.LastIndexByte(line, '}')
			if closing < open {
				return nil, fmt.Errorf("malformed sample: %s", line)
			}
			s.name = line[:open]
			if err := parseLabels(line[open+1:closing], s.labels); err != nil {
				return nil, fmt.Errorf("%w in: %s", err, line)
			}
			rest = line[closing+1:]
		} else {
			name, tail, _ := strings.Cut(line, " ")
			s.name, rest = name, tail
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return nil, fmt.Errorf("sample without a value: %s", line)
		}
		value, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			return nil, fmt.Errorf("sample value: %w in: %s", err, line)
		}
		s.value = value
		out = append(out, s)
	}
	return out, nil
}

func parseLabels(text string, into map[string]string) error {
	for text != "" {
		name, rest, ok := strings.Cut(text, `="`)
		if !ok {
			return fmt.Errorf("malformed labels")
		}
		var value strings.Builder
		i := 0
		for ; i < len(rest) && rest[i] != '"'; i++ {
			if rest[i] == '\\' && i+1 < len(rest) {
				i++
				if rest[i] == 'n' {
					value.WriteByte('\n')
					continue
				}
			}
			value.WriteByte(rest[i])
		}
		if i >= len(rest) {
			return fmt.Errorf("unterminated label value")
		}
		into[strings.TrimSpace(name)] = value.String()
		text = strings.TrimPrefix(strings.TrimSpace(rest[i+1:]), ",")
	}
	return nil
}

// find returns the samples of a series that carry the given labels.
func (s samples) find(name string, labels map[string]string) samples {
	var out samples
next:
	for _, candidate := range s {
		if candidate.name != name {
			continue
		}
		for k, v := range labels {
			if candidate.labels[k] != v {
				continue next
			}
		}
		out = append(out, candidate)
	}
	return out
}

func (s samples) names(prefix string) []string {
	seen := map[string]bool{}
	var out []string
	for _, candidate := range s {
		if strings.HasPrefix(candidate.name, prefix) && !seen[candidate.name] {
			seen[candidate.name] = true
			out = append(out, candidate.name)
		}
	}
	return out
}

// up checks that the collector reports the database reachable, under the
// labels the operator promises: dbname is the monitor's name.
func up(monitor string) func(samples) error {
	return func(got samples) error {
		found := got.find("pgwatch_instance_up", map[string]string{"dbname": monitor})
		if len(found) != 1 || found[0].value != 1 {
			return fmt.Errorf("pgwatch_instance_up{dbname=%q} is not 1: %+v", monitor, found)
		}
		return nil
	}
}

// present checks that every named series has a sample for the monitor,
// carrying the monitor's custom tags.
func present(monitor *pgpilotv1.PgpilotMonitor, series ...string) func(samples) error {
	return func(got samples) error {
		labels := map[string]string{"dbname": monitor.Name}
		maps.Copy(labels, monitor.Spec.Database.CustomTags)
		for _, name := range series {
			if len(got.find(name, labels)) == 0 {
				return fmt.Errorf("no sample of %s%v; the collector serves: %v", name, labels, got.names("pgwatch_"))
			}
		}
		return nil
	}
}

func all(checks ...func(samples) error) func(samples) error {
	return func(got samples) error {
		for _, check := range checks {
			if err := check(got); err != nil {
				return err
			}
		}
		return nil
	}
}

// fetchFailure matches the metric pgwatch could not fetch, in the two forms
// it logs: "failed to fetch metric <name>: fetch <name>: ERROR: ..." for the
// metrics of a batch (several of them in one record), and
// "[error:fetch <name>: ...]" when a metric is retried on its own. The metric
// named in the error text is the one that failed; the "metric" field of the
// same line can carry the name of another one.
var fetchFailure = regexp.MustCompile(`(?:failed to fetch metric |\[error:fetch )([a-z0-9_]+): `)

// failedMetrics returns the metrics whose query failed, from the log.
func (c *collector) failedMetrics() []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range fetchFailure.FindAllStringSubmatch(c.logs(), -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	slices.Sort(out)
	return out
}

// failureOf returns what the server answered for a metric that failed.
func (c *collector) failureOf(metric string) string {
	m := regexp.MustCompile(`fetch ` + regexp.QuoteMeta(metric) + `: ([^\]\n]*)`).FindStringSubmatch(c.logs())
	if m == nil {
		return "(no error text in the log)"
	}
	return m[1]
}

// awaitFirstCollection waits until the collector has been through all its
// metrics once: it has logged the outcome of a fetch, and then nothing new
// about failures for a few seconds (failed metrics are retried one by one
// right after the batch they were part of). A collector whose metrics all
// return no rows logs nothing at all; for that case the wait is bounded.
func (c *collector) awaitFirstCollection() {
	c.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(c.logs(), "measurements fetched") && len(c.failedMetrics()) == 0 {
		if !c.running() {
			c.t.Fatalf("the collector exited")
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	for settled, last := 0, -1; settled < 3; {
		time.Sleep(2 * time.Second)
		if now := len(c.failedMetrics()); now == last {
			settled++
		} else {
			settled, last = 0, now
		}
	}
}

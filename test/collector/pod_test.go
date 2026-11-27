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
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
	"github.com/jamalshahverdiev/pgpilot-operator/internal/builder"
)

// On OpenShift the pod names no user, and the cluster gives it one the image
// has never heard of. Everything the collector reads is mounted for a user
// that is not its owner: the configuration, the certificates, /tmp.
func TestTheCollectorRunsAsAUserThePlatformAssigns(t *testing.T) {
	ca, err := postgresAuthority()
	if err != nil {
		t.Fatal(err)
	}
	t.Parallel()
	monitor := newMonitor("platform-user", pgpilotv1.MetricsSpec{
		Preset: "basic",
		// A metric of its own, so that metrics.yaml is read as well.
		Custom: []pgpilotv1.MetricDefinition{{
			Name: "one", SQLs: map[string]string{"14": "SELECT 1 AS value"}, Gauges: []string{"*"},
		}},
	})
	monitor.Spec.Database.SSLMode = pgpilotv1.SSLModeVerifyFull
	monitor.Spec.Database.TLS = &pgpilotv1.DatabaseTLS{CASecretRef: &pgpilotv1.CASecretRef{Name: "pg-ca"}}
	held := defaultSecrets()
	held["pg-ca"] = map[string]string{"ca.crt": string(ca.pem)}

	c := startCollector(t, monitor, nil, held, builder.RunAsUser(nil))
	c.eventually("first samples", up(monitor.Name))
	out, err := docker("exec", c.name, "id")
	if err != nil {
		// The image has no shell and may have no id: what the runtime
		// says is as good.
		out, err = docker("inspect", "--format", "{{.Config.User}}", c.name)
	}
	if err != nil || !strings.Contains(out, "1000680000") {
		t.Errorf("the collector runs as %q (%v), want the user the platform assigned", strings.TrimSpace(out), err)
	}
	c.expectCleanLog()
}

// spec.env reaches the collector. A setting of the PostgreSQL driver is put
// to the server at every connection, where a metric of the monitor's own can
// read it back; a variable that says what the connection string says as well
// loses to it.
func TestEnvironmentVariablesOfTheMonitor(t *testing.T) {
	t.Parallel()
	monitor := newMonitor("with-env", pgpilotv1.MetricsSpec{
		Preset: "basic",
		Custom: []pgpilotv1.MetricDefinition{{
			Name:     "env_marker",
			Interval: &metav1.Duration{Duration: 5 * time.Second},
			SQLs: map[string]string{"14": "SELECT (extract(epoch from now()) * 1e9)::int8 AS epoch_ns, " +
				"current_setting('pgpilot.marker', true)::int8 AS marker"},
			Gauges: []string{"*"},
		}},
	})
	monitor.Spec.Env = []pgpilotv1.EnvVar{
		{Name: "PGOPTIONS", Value: "-c pgpilot.marker=4711"},
		// The connection string says which database; the variable must not.
		{Name: "PGDATABASE", Value: "no_such_database"},
		// pgwatch names its sessions itself: this one changes nothing,
		// which is why the documentation does not offer it.
		{Name: "PGAPPNAME", Value: "pgpilot-env-test"},
	}
	c := startCollector(t, monitor, nil, defaultSecrets())
	c.eventually("the setting of PGOPTIONS, read back by a metric", func(got samples) error {
		marker := got.find("pgwatch_env_marker_marker", map[string]string{"dbname": monitor.Name})
		if len(marker) != 1 || marker[0].value != 4711 {
			return fmt.Errorf("pgwatch_env_marker_marker is %v, want one sample of 4711", marker)
		}
		return nil
	})

	out, err := psql("SELECT count(*) FROM pg_stat_activity WHERE application_name = 'pgpilot-env-test'")
	if err != nil {
		t.Fatalf("pg_stat_activity: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != "0" {
		t.Errorf("PGAPPNAME names the sessions of the collector after all: say so in the description of spec.env")
	}
	c.expectCleanLog()
}

// A setting of pgwatch that has no field in the monitor reaches it through
// spec.env. The one tried here is one whose effect shows: a database below
// the size it names is not monitored at all.
func TestASettingOfPgwatchFromTheEnvironment(t *testing.T) {
	t.Parallel()
	monitor := newMonitor("pgwatch-env", pgpilotv1.MetricsSpec{Preset: "basic"})
	monitor.Spec.Env = []pgpilotv1.EnvVar{{Name: "PW_MIN_DB_SIZE_MB", Value: "1000000"}}
	c := startCollector(t, monitor, nil, defaultSecrets())
	for deadline := time.Now().Add(firstSamples); ; time.Sleep(time.Second) {
		logs := c.logs()
		if strings.Contains(logs, "ignored due to the --min-db-size-mb filter") {
			break
		}
		if strings.Contains(logs, "Connect OK") {
			t.Fatalf("the collector monitors a database far below PW_MIN_DB_SIZE_MB:\n%s", logs)
		}
		if time.Now().After(deadline) || !c.running() {
			t.Fatalf("the collector did not say that it leaves the database alone:\n%s", logs)
		}
	}
}

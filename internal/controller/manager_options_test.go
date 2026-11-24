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

package controller

import (
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
	"github.com/jamalshahverdiev/pgpilot-operator/internal/builder"
)

func TestOptions_Validate(t *testing.T) {
	tests := []struct {
		name    string
		options Options
		wantErr string
	}{
		{name: "zero values mean the defaults", options: Options{}},
		{name: "the production default", options: Options{CredentialsRecheck: 5 * time.Minute}},
		{name: "exactly the minimum", options: Options{CredentialsRecheck: MinCredentialsRecheck}},
		{name: "a long interval", options: Options{CredentialsRecheck: 24 * time.Hour}},
		{
			name:    "just below the minimum",
			options: Options{CredentialsRecheck: MinCredentialsRecheck - time.Nanosecond},
			wantErr: "too short",
		},
		// One millisecond was measured at five full reconciles per second
		// for a single monitor.
		{name: "a busy loop", options: Options{CredentialsRecheck: time.Millisecond}, wantErr: "too short"},
		{name: "negative recheck", options: Options{CredentialsRecheck: -5 * time.Second}, wantErr: "too short"},
		{name: "negative retry", options: Options{CredentialsRetry: -time.Second}, wantErr: "negative"},
		{name: "short retry is fine", options: Options{CredentialsRetry: time.Second}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.options.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("Validate() = %v, want no error", err)
			case tt.wantErr != "" && err == nil:
				t.Errorf("Validate() = nil, want an error containing %q", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Errorf("Validate() = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func cachedKinds(options cache.Options) []string {
	kinds := make([]string, 0, len(options.ByObject))
	for obj := range options.ByObject {
		kinds = append(kinds, reflect.TypeOf(obj).Elem().Name())
	}
	sort.Strings(kinds)
	return kinds
}

func selectorFor(t *testing.T, options cache.Options, kind client.Object) labels.Selector {
	t.Helper()
	for obj, by := range options.ByObject {
		if reflect.TypeOf(obj) == reflect.TypeOf(kind) {
			if by.Label == nil {
				t.Fatalf("%T is cached without a label selector", kind)
			}
			return by.Label
		}
	}
	t.Fatalf("%T is not restricted by the cache options", kind)
	return nil
}

// A kind missing here is cached for the whole cluster again.
func TestCacheOptions_RestrictEveryKindTheManagerReads(t *testing.T) {
	without := []string{"ConfigMap", "Deployment", "Pod", "Service"}
	if got := cachedKinds(CacheOptions(false)); !reflect.DeepEqual(got, without) {
		t.Errorf("restricted kinds = %v, want %v", got, without)
	}
	with := []string{"ConfigMap", "Deployment", "Pod", "Service", "ServiceMonitor"}
	if got := cachedKinds(CacheOptions(true)); !reflect.DeepEqual(got, with) {
		t.Errorf("restricted kinds with ServiceMonitors = %v, want %v", got, with)
	}
	if CacheOptions(true).DefaultTransform == nil {
		t.Error("managedFields are no longer stripped from cached objects")
	}
}

// The cache only holds objects that match its label selector. Should a
// generated object stop carrying the label, the operator would no longer see
// what it created: the Deployment's status, the pod's name, a deleted Service.
func TestCacheOptions_SelectEveryGeneratedObject(t *testing.T) {
	monitor := &pgpilotv1.PgpilotMonitor{}
	monitor.Name, monitor.Namespace = "orders", "team"
	monitor.Spec.Database = pgpilotv1.DatabaseRef{
		Host: "pg", Database: "db", CredentialsSecret: &pgpilotv1.CredentialsSecretRef{Name: "creds"},
	}
	monitor.Spec.Sinks = pgpilotv1.SinksSpec{Prometheus: &pgpilotv1.PrometheusSink{Enabled: true}}
	// A user may add labels, including one that tries to take the place of
	// the operator's own.
	hostile := map[string]pgpilotv1.LabelValue{
		builder.LabelAppManagedBy: "someone-else",
		"team":                    "data",
	}
	monitor.Spec.PodMetadata.Labels = hostile
	monitor.Spec.ServiceMetadata.Labels = hostile

	merged := builder.MergeMetrics(pgpilotv1.MetricsSpec{Preset: "basic"}, nil)
	configMap, _, err := builder.BuildConfigMap(monitor, merged)
	if err != nil {
		t.Fatal(err)
	}
	deployment := builder.BuildDeployment(monitor, merged, builder.PodTemplateVersions{Config: "h"})
	service := builder.BuildService(monitor)
	serviceMonitor := builder.BuildServiceMonitor(monitor)

	options := CacheOptions(true)
	generated := []struct {
		name   string
		kind   client.Object
		labels map[string]string
	}{
		{"ConfigMap", &corev1.ConfigMap{}, configMap.Labels},
		{"Deployment", &appsv1.Deployment{}, deployment.Labels},
		{"pods of the Deployment", &corev1.Pod{}, deployment.Spec.Template.Labels},
		{"Service", &corev1.Service{}, service.Labels},
		{"ServiceMonitor", &monitoringv1.ServiceMonitor{}, serviceMonitor.Labels},
	}
	for _, g := range generated {
		t.Run(g.name, func(t *testing.T) {
			selector := selectorFor(t, options, g.kind)
			if !selector.Matches(labels.Set(g.labels)) {
				t.Errorf("labels %v are not selected by %q: the operator would not see this object", g.labels, selector)
			}
		})
	}

	t.Run("objects of others are not selected", func(t *testing.T) {
		selector := selectorFor(t, options, &corev1.ConfigMap{})
		for _, foreign := range []labels.Set{
			nil,
			{"app": "postgres"},
			{builder.LabelAppManagedBy: "Helm"},
			{builder.LabelAppName: builder.AppName},
		} {
			if selector.Matches(foreign) {
				t.Errorf("labels %v are selected although the operator did not create the object", foreign)
			}
		}
	})
}

func TestClientOptions_NeverCacheSecrets(t *testing.T) {
	options := ClientOptions()
	if options.Cache == nil || len(options.Cache.DisableFor) != 1 {
		t.Fatalf("ClientOptions().Cache = %+v, want the cache disabled for exactly one kind", options.Cache)
	}
	if _, isSecret := options.Cache.DisableFor[0].(*corev1.Secret); !isSecret {
		t.Errorf("the cache is disabled for %T, want *corev1.Secret", options.Cache.DisableFor[0])
	}
}

// Which updates of a monitor reconcile it. A status write must not: every
// reconcile ends with one, and each used to cause the next.
func TestMonitorChanged(t *testing.T) {
	base := func() *pgpilotv1.PgpilotMonitor {
		m := &pgpilotv1.PgpilotMonitor{}
		m.Name, m.Namespace, m.Generation, m.ResourceVersion = "db", "team", 4, "100"
		m.Labels = map[string]string{"team": "data"}
		m.Annotations = map[string]string{"note": "a"}
		return m
	}
	tests := []struct {
		name   string
		change func(*pgpilotv1.PgpilotMonitor)
		want   bool
	}{
		{name: "spec changed (generation moves)", change: func(m *pgpilotv1.PgpilotMonitor) { m.Generation = 5 }, want: true},
		{
			name: "deletion started (generation moves)",
			change: func(m *pgpilotv1.PgpilotMonitor) {
				now := metav1.Now()
				m.DeletionTimestamp, m.Generation = &now, 5
			},
			want: true,
		},
		{name: "annotation added", change: func(m *pgpilotv1.PgpilotMonitor) { m.Annotations["reconcile"] = "now" }, want: true},
		{name: "annotation changed", change: func(m *pgpilotv1.PgpilotMonitor) { m.Annotations["note"] = "b" }, want: true},
		{name: "label changed", change: func(m *pgpilotv1.PgpilotMonitor) { m.Labels["team"] = "platform" }, want: true},
		{
			name: "status written",
			change: func(m *pgpilotv1.PgpilotMonitor) {
				now := metav1.Now()
				m.Status.Ready, m.Status.LastReconciled, m.Status.ConfigHash = true, &now, "abc"
			},
		},
		{name: "finalizer added", change: func(m *pgpilotv1.PgpilotMonitor) { m.Finalizers = []string{finalizerName} }},
		{name: "nothing but the resourceVersion", change: func(*pgpilotv1.PgpilotMonitor) {}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, after := base(), base()
			tt.change(after)
			after.ResourceVersion = "101"
			if got := monitorChanged().Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after}); got != tt.want {
				t.Errorf("reconcile on this update = %v, want %v", got, tt.want)
			}
		})
	}

	t.Run("a new or deleted monitor always reconciles", func(t *testing.T) {
		if !monitorChanged().Create(event.CreateEvent{Object: base()}) {
			t.Error("create events must pass")
		}
		if !monitorChanged().Delete(event.DeleteEvent{Object: base()}) {
			t.Error("delete events must pass")
		}
	})
}

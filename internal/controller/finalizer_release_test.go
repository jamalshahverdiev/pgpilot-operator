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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
	"github.com/jamalshahverdiev/pgpilot-operator/internal/builder"
)

// sentPatches is a client that keeps the patches it is sent, per object, and
// answers each with what fail returns.
type sentPatches struct {
	client.Client
	byName map[string][]map[string]any
	order  []string
	// all holds every patch that was accepted, in the order sent.
	all  [][]map[string]any
	fail func(name string) error
}

func capturePatches(t *testing.T) *sentPatches {
	t.Helper()
	s := &sentPatches{byName: map[string][]map[string]any{}}
	s.Client = fake.NewClientBuilder().WithScheme(migrationScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, patch client.Patch, _ ...client.PatchOption) error {
				if patch.Type() != types.JSONPatchType {
					t.Errorf("patch type %s: the object must not be sent back", patch.Type())
				}
				data, err := patch.Data(obj)
				if err != nil {
					return err
				}
				var ops []map[string]any
				if err := json.Unmarshal(data, &ops); err != nil {
					return err
				}
				s.order = append(s.order, obj.GetName())
				if s.fail != nil {
					if err := s.fail(obj.GetName()); err != nil {
						return err
					}
				}
				s.byName[obj.GetName()] = ops
				s.all = append(s.all, ops)
				// A removed tag stays removed: as the API server answers
				// under the current CRD.
				if slices.ContainsFunc(ops, func(o map[string]any) bool {
					return o["op"] == "remove" && o["path"] == "/spec/image/tag"
				}) {
					obj.(*pgpilotv1.PgpilotMonitor).Spec.Image.Tag = ""
				}
				return nil
			},
		}).Build()
	return s
}

func op(kind, path string, value ...any) map[string]any {
	out := map[string]any{"op": kind, "path": path}
	if len(value) == 1 {
		out["value"] = value[0]
	}
	return out
}

func sameOps(got, want []map[string]any) bool {
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(want)
	return string(a) == string(b)
}

const noteAnnotationPath = "/metadata/annotations/pgpilot.io~1operator-wrote-image-tag"

func TestFinalizerRelease_WhatIsSent(t *testing.T) {
	// A monitor a release before v1.1 reconciled and whose spec nobody else
	// has written: the tag is that release's.
	legacy := func(change ...func(*pgpilotv1.PgpilotMonitor)) *pgpilotv1.PgpilotMonitor {
		m := legacyMonitor("db")
		m.ResourceVersion = "41"
		for _, c := range change {
			c(m)
		}
		return m
	}
	// Nothing about the version of the object: a status write by the
	// reconciler in between must not fail the patch.
	release := []map[string]any{
		op("test", "/metadata/finalizers/0", "pgpilot.io/finalizer"),
		op("remove", "/metadata/finalizers/0"),
	}
	// The note is only written while the tag is still the one it is about.
	with := func(note ...map[string]any) []map[string]any {
		return append(append(slices.Clone(release), op("test", "/spec/image/tag", "5.1.0")), note...)
	}
	// A monitor that was read without annotations is given the map, and only
	// while it still has none: adding the map replaces one that is there. The
	// null is spelled out in what is sent.
	firstAnnotation := []map[string]any{
		op("test", "/metadata/annotations", nil),
		op("add", "/metadata/annotations", map[string]any{"pgpilot.io/operator-wrote-image-tag": "5.1.0"}),
	}
	tagOnly := managedBy("manager", metav1.ManagedFieldsOperationUpdate, "", `{"f:spec":{"f:image":{"f:repository":{},"f:tag":{}}}}`)
	finalizerByAnother := managedBy("kubectl-patch", metav1.ManagedFieldsOperationUpdate, "",
		`{"f:metadata":{"f:finalizers":{"v:\"pgpilot.io/finalizer\"":{}}}}`)

	cases := map[string]struct {
		monitor *pgpilotv1.PgpilotMonitor
		want    []map[string]any // nil: nothing is sent
	}{
		// The image migration is off, or left the tag for a reason that can
		// change: what the finalizer proved is written down.
		"the operator's tag is still there": {legacy(), with(firstAnnotation...)},
		"the same, on a monitor that has annotations": {
			legacy(func(m *pgpilotv1.PgpilotMonitor) { m.Annotations = map[string]string{"team": "data"} }),
			with(op("add", noteAnnotationPath, "5.1.0")),
		},
		"noted already": {
			legacy(func(m *pgpilotv1.PgpilotMonitor) {
				m.Annotations = map[string]string{AnnotationOperatorWroteImageTag: "5.1.0"}
			}),
			release,
		},
		"a note about another tag does not count": {
			legacy(func(m *pgpilotv1.PgpilotMonitor) {
				m.Annotations = map[string]string{AnnotationOperatorWroteImageTag: "4.9.0"}
			}),
			with(op("add", noteAnnotationPath, "5.1.0")),
		},
		"the tag has been migrated": {
			legacy(func(m *pgpilotv1.PgpilotMonitor) { m.Spec.Image = pgpilotv1.ImageSpec{} }),
			release,
		},
		"another tag": {
			legacy(func(m *pgpilotv1.PgpilotMonitor) { m.Spec.Image.Tag = "6.2.0" }),
			release,
		},
		// The record does not say the operator wrote the tag: nothing to note.
		"the tag is another writer's": {
			legacy(func(m *pgpilotv1.PgpilotMonitor) {
				m.ManagedFields = []metav1.ManagedFieldsEntry{
					managedBy("manager", metav1.ManagedFieldsOperationUpdate, "",
						`{"f:metadata":{"f:finalizers":{"v:\"pgpilot.io/finalizer\"":{}}}}`),
					managedBy("kubectl-edit", metav1.ManagedFieldsOperationUpdate, "", `{"f:spec":{"f:image":{"f:tag":{}}}}`),
				}
			}),
			release,
		},
		// Written by the operator and confirmed by someone else since: the
		// image migration keeps it for now, and can still tell later.
		"the tag is the operator's and another writer's": {
			legacy(func(m *pgpilotv1.PgpilotMonitor) {
				m.ManagedFields = append(m.ManagedFields,
					managedBy("helm", metav1.ManagedFieldsOperationApply, "", `{"f:spec":{"f:image":{"f:tag":{}}}}`))
			}),
			with(firstAnnotation...),
		},
		// A writer called "manager" need not be the operator. Only the
		// finalizer in the same entry says so; the note would say it for
		// good, and the image migration would take a tag that is not the
		// operator's to remove.
		`a "manager" that owns the tag but not the finalizer`: {
			legacy(func(m *pgpilotv1.PgpilotMonitor) {
				m.ManagedFields = []metav1.ManagedFieldsEntry{tagOnly, finalizerByAnother}
			}),
			release,
		},
		"the same, with another writer on the tag as well": {
			legacy(func(m *pgpilotv1.PgpilotMonitor) {
				m.ManagedFields = []metav1.ManagedFieldsEntry{tagOnly, finalizerByAnother,
					managedBy("helm", metav1.ManagedFieldsOperationApply, "", `{"f:spec":{"f:image":{"f:tag":{}}}}`)}
			}),
			release,
		},
		"no ownership record": {
			legacy(func(m *pgpilotv1.PgpilotMonitor) { m.ManagedFields = nil }),
			release,
		},
		"the finalizer is not the first one": {
			legacy(func(m *pgpilotv1.PgpilotMonitor) {
				m.Finalizers = []string{"example.com/backup", finalizerName, "example.com/audit"}
				m.Spec.Image = pgpilotv1.ImageSpec{}
			}),
			[]map[string]any{
				op("test", "/metadata/finalizers/1", "pgpilot.io/finalizer"),
				op("remove", "/metadata/finalizers/1"),
			},
		},
		"no finalizer": {legacy(func(m *pgpilotv1.PgpilotMonitor) { m.Finalizers = nil }), nil},
		"only other finalizers": {
			legacy(func(m *pgpilotv1.PgpilotMonitor) { m.Finalizers = []string{"example.com/backup"} }), nil,
		},
		// The reconciler lets it go at once; see Reconcile.
		"being deleted": {
			legacy(func(m *pgpilotv1.PgpilotMonitor) {
				now := metav1.Now()
				m.DeletionTimestamp = &now
			}),
			nil,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := capturePatches(t)
			released, err := (&FinalizerRelease{Reader: &monitorLister{monitors: monitors(tc.monitor)}, Client: c}).
				Run(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			got := c.byName["db"]
			if !sameOps(got, tc.want) {
				t.Errorf("sent\n  %v\nwant\n  %v", got, tc.want)
			}
			if wantReleased := len(tc.want) > 0; (released == 1) != wantReleased || released > 1 {
				t.Errorf("released %d", released)
			}
		})
	}

	t.Run("an ownership record that cannot be read is an error, and the finalizer stays", func(t *testing.T) {
		monitor := legacy(func(m *pgpilotv1.PgpilotMonitor) {
			m.ManagedFields = []metav1.ManagedFieldsEntry{
				managedBy("manager", metav1.ManagedFieldsOperationUpdate, "", `{"f:spec":`)}
		})
		c := capturePatches(t)
		released, err := (&FinalizerRelease{Reader: &monitorLister{monitors: monitors(monitor)}, Client: c}).
			Run(context.Background())
		if err == nil || released != 0 || len(c.order) != 0 {
			t.Errorf("released %d, sent %v, error %v", released, c.order, err)
		}
	})
}

// namesExactly reports whether err is about these monitors and no others.
func namesExactly(err error, monitors ...string) bool {
	for _, name := range monitors {
		if !strings.Contains(err.Error(), name+": ") {
			return false
		}
	}
	return strings.Count(err.Error(), "default/") == len(monitors)
}

// The patch as the API server applies it: the fake client evaluates the
// tests and removes what it is told to, like the real one.
func TestFinalizerRelease_AgainstStoredObjects(t *testing.T) {
	stored := func(name string, finalizers ...string) *pgpilotv1.PgpilotMonitor {
		m := &pgpilotv1.PgpilotMonitor{}
		m.Name, m.Namespace, m.Finalizers = name, "default", finalizers
		m.Labels = map[string]string{"team": "data"}
		m.Spec.Database = pgpilotv1.DatabaseRef{Host: "pg", Database: "app"}
		return m
	}
	noted := stored("noted", finalizerName)
	noted.Spec.Image.Tag = legacyImageTag
	retagged := stored("retagged", finalizerName)
	retagged.Spec.Image.Tag = legacyImageTag
	annotated := stored("annotated-since", finalizerName)
	annotated.Spec.Image.Tag = legacyImageTag
	c := fake.NewClientBuilder().WithScheme(migrationScheme(t)).WithObjects(
		stored("only-ours", finalizerName),
		stored("among-others", "example.com/backup", finalizerName, "example.com/audit"),
		stored("none"),
		stored("status-written", finalizerName),
		stored("list-changed", "example.com/backup", finalizerName),
		noted, retagged, annotated,
	).WithStatusSubresource(&pgpilotv1.PgpilotMonitor{}).Build()
	get := func(name string) *pgpilotv1.PgpilotMonitor {
		t.Helper()
		m := &pgpilotv1.PgpilotMonitor{}
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, m); err != nil {
			t.Fatal(err)
		}
		return m
	}

	// What the migration is handed: the objects as they were listed, with
	// the ownership record the fake client does not keep.
	names := []string{
		"only-ours", "among-others", "none", "status-written", "list-changed", "noted", "retagged", "annotated-since",
	}
	listed := make([]*pgpilotv1.PgpilotMonitor, 0, len(names))
	for _, name := range names {
		m := get(name)
		if m.Spec.Image.Tag == legacyImageTag {
			m.ManagedFields = legacyMonitor(name).ManagedFields
		}
		listed = append(listed, m)
	}
	// Since the list: the reconciler wrote a status,
	written := get("status-written")
	written.Status.ConfigHash = "abc"
	if err := c.Status().Update(context.Background(), written); err != nil {
		t.Fatal(err)
	}
	// someone took another finalizer off, which moved the operator's,
	shifted := get("list-changed")
	shifted.Finalizers = []string{finalizerName}
	if err := c.Update(context.Background(), shifted); err != nil {
		t.Fatal(err)
	}
	// someone chose another tag,
	changed := get("retagged")
	changed.Spec.Image.Tag = "6.2.0"
	if err := c.Update(context.Background(), changed); err != nil {
		t.Fatal(err)
	}
	// and someone gave a monitor that had no annotations its first ones.
	theirs := map[string]string{AnnotationKeepImageTag: "true", "team": "payments"}
	first := get("annotated-since")
	first.Annotations = theirs
	if err := c.Update(context.Background(), first); err != nil {
		t.Fatal(err)
	}

	released, err := (&FinalizerRelease{Reader: &monitorLister{monitors: listed}, Client: c}).Run(context.Background())
	if released != 4 {
		t.Errorf("released %d, want only-ours, among-others, status-written and noted", released)
	}
	if err == nil || !namesExactly(err, "default/list-changed", "default/retagged", "default/annotated-since") {
		t.Errorf("want an error about the three monitors whose patch no longer fits, and no other; got %v", err)
	}

	for name, want := range map[string][]string{
		"only-ours":       nil,
		"among-others":    {"example.com/backup", "example.com/audit"},
		"none":            nil,
		"status-written":  nil,
		"list-changed":    {finalizerName},
		"noted":           nil,
		"retagged":        {finalizerName},
		"annotated-since": {finalizerName},
	} {
		m := get(name)
		if !slices.Equal(m.Finalizers, want) {
			t.Errorf("%s: finalizers %v, want %v", name, m.Finalizers, want)
		}
		if m.Labels["team"] != "data" || m.Spec.Database.Host != "pg" {
			t.Errorf("%s: something else changed: %+v", name, m)
		}
	}
	if got := get("status-written").Status.ConfigHash; got != "abc" {
		t.Errorf("the status written meanwhile is gone: %q", got)
	}
	if got := get("noted").Annotations; got[AnnotationOperatorWroteImageTag] != "5.1.0" || len(got) != 1 {
		t.Errorf("noted: annotations %v", got)
	}
	if got := get("retagged").Annotations; len(got) != 0 {
		t.Errorf("retagged: a note about a tag that is no longer there: %v", got)
	}
	// The note is added as the whole map only to a monitor that has none.
	// Sent to this one it would have taken the place of what was written
	// since, the request to keep the tag among it.
	if got := get("annotated-since").Annotations; !maps.Equal(got, theirs) {
		t.Errorf("annotated-since: annotations %v, want those written since the list: %v", got, theirs)
	}

	// The next pass reads them again and finds them as they are now.
	released, err = (&FinalizerRelease{Reader: c, Client: c}).Run(context.Background())
	if released != 3 || err != nil {
		t.Errorf("second pass: released %d, %v", released, err)
	}
	if m := get("annotated-since"); len(m.Finalizers) != 0 || !maps.Equal(m.Annotations, theirs) {
		t.Errorf("annotated-since after the second pass: finalizers %v, annotations %v", m.Finalizers, m.Annotations)
	}
	for _, name := range []string{"list-changed", "retagged"} {
		if m := get(name); len(m.Finalizers) != 0 || len(m.Annotations) != 0 {
			t.Errorf("%s after the second pass: finalizers %v, annotations %v", name, m.Finalizers, m.Annotations)
		}
	}
}

func TestFinalizerRelease_RunCarriesOnPastAFailure(t *testing.T) {
	c := capturePatches(t)
	c.fail = func(name string) error {
		switch name {
		case "b":
			return errors.New("admission said no")
		case "c":
			// Deleted between the list and the patch: nothing left to do.
			return apierrors.NewNotFound(schema.GroupResource{Resource: "pgpilotmonitors"}, name)
		}
		return nil
	}
	reader := &monitorLister{pageSize: 2, monitors: monitors(
		legacyMonitor("a"), legacyMonitor("b"), legacyMonitor("c"), legacyMonitor("d"), legacyMonitor("e"))}

	released, err := (&FinalizerRelease{Reader: reader, Client: c, PageSize: 2}).Run(context.Background())
	if released != 3 || !slices.Equal(c.order, []string{"a", "b", "c", "d", "e"}) {
		t.Errorf("released %d, tried %v; want every monitor tried and three released", released, c.order)
	}
	if err == nil || !strings.Contains(err.Error(), "default/b") || !strings.Contains(err.Error(), "admission said no") ||
		strings.Contains(err.Error(), "default/c") {
		t.Errorf("want an error that names the monitor that failed and only that one, got %v", err)
	}
	if reader.calls != 3 || !slices.Equal(reader.limits, []int64{2, 2, 2}) || !slices.Equal(reader.tokens, []string{"", "2", "4"}) {
		t.Errorf("list calls %d, limits %v, tokens %v", reader.calls, reader.limits, reader.tokens)
	}

	t.Run("pages of 200 unless told otherwise", func(t *testing.T) {
		reader := &monitorLister{}
		if _, err := (&FinalizerRelease{Reader: reader, Client: capturePatches(t)}).Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(reader.limits, []int64{200}) {
			t.Errorf("limits %v", reader.limits)
		}
	})

	t.Run("a list that fails ends the pass with what was done, and with what failed before it", func(t *testing.T) {
		boom := errors.New("etcd is unavailable")
		reader := &monitorLister{pageSize: 1, monitors: monitors(legacyMonitor("a"), legacyMonitor("b"), legacyMonitor("c")),
			fail: func(call int) error {
				if call == 3 {
					return boom
				}
				return nil
			}}
		c := capturePatches(t)
		c.fail = func(name string) error {
			if name == "b" {
				return errors.New("admission said no")
			}
			return nil
		}
		released, err := (&FinalizerRelease{Reader: reader, Client: c}).Run(context.Background())
		if released != 1 || !errors.Is(err, boom) || !strings.Contains(err.Error(), "default/b: ") {
			t.Errorf("released %d, %v", released, err)
		}
	})
}

// A pass can fail for every monitor of a cluster, and it is made six times.
// The log is told how many failed and shown the first of them.
func TestFinalizerRelease_WhatAFailedPassLogs(t *testing.T) {
	var all []*pgpilotv1.PgpilotMonitor
	for i := 1; i <= keptInLog+5; i++ {
		all = append(all, legacyMonitor(fmt.Sprintf("m%02d", i)))
	}
	c := capturePatches(t)
	c.fail = func(string) error { return errors.New("is invalid") }
	var out bytes.Buffer
	ctx := logf.IntoContext(context.Background(), zap.New(zap.WriteTo(&out), zap.UseDevMode(false)))
	backoff := wait.Backoff{Duration: time.Millisecond, Factor: 1, Steps: 2}

	if err := (&FinalizerRelease{Reader: &monitorLister{monitors: all}, Client: c, Backoff: backoff}).Start(ctx); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("%d log lines, want one per pass and one at the end:\n%s", len(lines), out.String())
	}
	for i, line := range lines {
		if !strings.Contains(line, "default/m01: ") || !strings.Contains(line, fmt.Sprintf("default/m%02d: ", keptInLog)) ||
			strings.Contains(line, fmt.Sprintf("default/m%02d", keptInLog+1)) {
			t.Errorf("line %d does not name the first %d monitors and no more: %s", i, keptInLog, line)
		}
	}
	for _, line := range lines[:2] {
		if !strings.Contains(line, "will retry") || !strings.Contains(line, fmt.Sprintf(`"failures":%d`, keptInLog+5)) {
			t.Errorf("a failed pass logged: %s", line)
		}
	}
	// What the image migration says as well: retrying does not help a
	// monitor the API server refuses to change.
	if last := lines[2]; !strings.Contains(last, "giving up for now") || !strings.Contains(last, "spec corrected first") {
		t.Errorf("at the end: %s", last)
	}

	t.Run("an error that is not a list of them counts as one", func(t *testing.T) {
		boom := errors.New("boom")
		if n, err := firstFailures(boom); n != 1 || !errors.Is(err, boom) {
			t.Errorf("got %d, %v", n, err)
		}
		few := errors.Join(errors.New("a"), errors.New("b"))
		if n, err := firstFailures(few); n != 2 || err.Error() != few.Error() {
			t.Errorf("got %d, %v", n, err)
		}
	})
}

func TestFinalizerRelease_StartRetriesAndNeverFailsTheManager(t *testing.T) {
	backoff := wait.Backoff{Duration: time.Millisecond, Factor: 1, Steps: 4}

	t.Run("until a pass completes", func(t *testing.T) {
		c := capturePatches(t)
		attempts := 0
		c.fail = func(string) error {
			if attempts++; attempts < 3 {
				return errors.New("not yet")
			}
			return nil
		}
		reader := &monitorLister{monitors: monitors(legacyMonitor("a"))}
		if err := (&FinalizerRelease{Reader: reader, Client: c, Backoff: backoff}).Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		if reader.calls != 3 {
			t.Errorf("%d passes, want it to stop after the first that completed", reader.calls)
		}
	})

	t.Run("gives up after the last attempt without an error", func(t *testing.T) {
		c := capturePatches(t)
		c.fail = func(string) error { return errors.New("never") }
		reader := &monitorLister{monitors: monitors(legacyMonitor("a"))}
		if err := (&FinalizerRelease{Reader: reader, Client: c, Backoff: backoff}).Start(context.Background()); err != nil {
			t.Errorf("Start returned %v: that would stop the operator", err)
		}
		if reader.calls != 4 {
			t.Errorf("%d passes, want the %d of the schedule", reader.calls, backoff.Steps)
		}
	})

	// The manager builds it without a schedule of its own.
	t.Run("with the schedule of the image migration when it is given none", func(t *testing.T) {
		c := capturePatches(t)
		reader := &monitorLister{monitors: monitors(legacyMonitor("a"))}
		if err := (&FinalizerRelease{Reader: reader, Client: c}).Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		if reader.calls != 1 || len(c.order) != 1 {
			t.Errorf("%d passes, %d patches: want one pass", reader.calls, len(c.order))
		}
	})

	t.Run("stops when the manager does", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		reader := &monitorLister{fail: func(int) error { return errors.New("never") }}
		slow := wait.Backoff{Duration: time.Hour, Factor: 1, Steps: 100}
		done := make(chan error, 1)
		go func() {
			done <- (&FinalizerRelease{Reader: reader, Client: capturePatches(t), Backoff: slow}).Start(ctx)
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Start returned %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Start did not return after the context was cancelled")
		}
	})

	if !(&FinalizerRelease{}).NeedLeaderElection() || !(&legacyMigrations{}).NeedLeaderElection() {
		t.Error("only the leader may write")
	}
}

// The image tag is decided on while the ownership record is complete; only
// then does the finalizer go.
func TestLegacyMigrations_ImageFirstThenTheFinalizer(t *testing.T) {
	backoff := wait.Backoff{Duration: time.Millisecond, Factor: 1, Steps: 1}
	reader := &monitorLister{monitors: monitors(legacyMonitor("a"))}

	t.Run("both", func(t *testing.T) {
		c := capturePatches(t)
		l := &legacyMigrations{
			image:      &StoredImageMigration{Reader: reader, Client: c, Recorder: record.NewFakeRecorder(5), Backoff: backoff},
			finalizers: &FinalizerRelease{Reader: reader, Client: c, Backoff: backoff},
		}
		if err := l.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(c.all) != 2 {
			t.Fatalf("%d patches, want the image's and the finalizer's", len(c.all))
		}
		if first := c.all[0]; len(first) < 3 || first[2]["path"] != "/spec/image/tag" {
			t.Errorf("the first patch was %v, want the image's", first)
		}
		if second := c.all[1]; len(second) < 2 || second[1]["path"] != "/metadata/finalizers/0" {
			t.Errorf("the second patch was %v, want the finalizer's", second)
		}
	})

	t.Run("without the image migration the finalizer still goes", func(t *testing.T) {
		c := capturePatches(t)
		l := &legacyMigrations{finalizers: &FinalizerRelease{Reader: reader, Client: c, Backoff: backoff}}
		if err := l.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		ops := c.byName["a"]
		if len(c.order) != 1 || len(ops) != 5 || ops[1]["path"] != "/metadata/finalizers/0" ||
			ops[4]["op"] != "add" || ops[4]["path"] != "/metadata/annotations" {
			t.Errorf("sent %v", ops)
		}
	})
}

func TestMigrations_AreBuiltFromTheManager(t *testing.T) {
	reader := fake.NewClientBuilder().Build()
	var writtenAs []string
	writer := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, opts ...client.PatchOption) error {
			options := &client.PatchOptions{}
			for _, o := range opts {
				o.ApplyToPatch(options)
			}
			writtenAs = append(writtenAs, options.FieldManager)
			return nil
		},
	}).Build()
	src := &migrationSourceStub{reader: reader, recorder: record.NewFakeRecorder(1)}

	all := migrations(src, writer, Options{})
	if all.image == nil || all.finalizers == nil {
		t.Fatalf("both migrations must run by default: %+v", all)
	}
	// Reading through the cache would show no ownership record.
	if all.finalizers.Reader != reader {
		t.Error("the finalizer release must read through the manager's API reader")
	}
	monitor := &pgpilotv1.PgpilotMonitor{}
	monitor.Name, monitor.Namespace = "db", "team"
	if err := all.finalizers.Client.Patch(context.Background(), monitor, client.RawPatch(types.JSONPatchType, []byte("[]"))); err != nil {
		t.Fatal(err)
	}
	if len(writtenAs) != 1 || writtenAs[0] != "pgpilot-operator" {
		t.Errorf("written as %q, want one write as pgpilot-operator", writtenAs)
	}

	// Keeping the stored image tag is a choice about the image. The
	// finalizer goes all the same.
	kept := migrations(src, writer, Options{KeepStoredImageTag: true})
	if kept.image != nil || kept.finalizers == nil {
		t.Errorf("with the image migration off: %+v", kept)
	}
}

func TestJSONPointerToken(t *testing.T) {
	for in, want := range map[string]string{
		"plain":                               "plain",
		"pgpilot.io/operator-wrote-image-tag": "pgpilot.io~1operator-wrote-image-tag",
		"a~b":                                 "a~0b",
		"a~/b":                                "a~0~1b",
	} {
		if got := jsonPointerToken(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

// What the finalizer proved is still known to the image migration after the
// finalizer is gone.
func TestStoredImageMigration_TheNoteStandsInForTheFinalizer(t *testing.T) {
	// The record as the API server keeps it once the finalizer was removed
	// by another writer: the old operator still owns the tag, and nothing
	// ties that entry to the operator any more.
	afterRelease := func(annotations map[string]string) *pgpilotv1.PgpilotMonitor {
		m := legacyMonitor("db")
		m.ResourceVersion = "7"
		m.Finalizers = nil
		m.Annotations = annotations
		m.ManagedFields = []metav1.ManagedFieldsEntry{
			managedBy("manager", metav1.ManagedFieldsOperationUpdate, "",
				`{"f:spec":{"f:image":{"f:repository":{},"f:tag":{}}}}`),
			managedBy(fieldOwner, metav1.ManagedFieldsOperationUpdate, "",
				`{"f:metadata":{"f:annotations":{"f:pgpilot.io/operator-wrote-image-tag":{}}}}`),
		}
		return m
	}
	run := func(t *testing.T, m *pgpilotv1.PgpilotMonitor) (MigrationResult, *sentPatches) {
		t.Helper()
		c := capturePatches(t)
		result, err := (&StoredImageMigration{Reader: &monitorLister{monitors: monitors(m)}, Client: c}).
			Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return result, c
	}

	t.Run("with the note the tag is removed, and the note with it", func(t *testing.T) {
		result, c := run(t, afterRelease(map[string]string{AnnotationOperatorWroteImageTag: "5.1.0"}))
		if result.Migrated != 1 || len(result.Kept) != 0 {
			t.Fatalf("got %+v", result)
		}
		want := []map[string]any{
			op("test", "/metadata/resourceVersion", "7"),
			op("test", "/spec/image/tag", "5.1.0"),
			op("remove", "/spec/image/tag"),
			op("test", "/spec/image/repository", builder.DefaultImageRepository),
			op("remove", "/spec/image/repository"),
			op("remove", noteAnnotationPath),
		}
		if got := c.byName["db"]; !sameOps(got, want) {
			t.Errorf("sent\n  %v\nwant\n  %v", got, want)
		}
	})
	t.Run("without it nothing ties the tag to the operator", func(t *testing.T) {
		result, c := run(t, afterRelease(nil))
		if result.Migrated != 0 || len(result.Kept) != 1 || !strings.Contains(result.Kept[0], "not the operator") {
			t.Errorf("got %+v", result)
		}
		if len(c.order) != 0 {
			t.Errorf("sent %v", c.byName)
		}
	})
	t.Run("a note about another tag is no note", func(t *testing.T) {
		result, _ := run(t, afterRelease(map[string]string{AnnotationOperatorWroteImageTag: "4.9.0"}))
		if result.Migrated != 0 || len(result.Kept) != 1 {
			t.Errorf("got %+v", result)
		}
	})
	// The note stands in for the finalizer and for nothing else. The release
	// writes it onto a tag that Helm applied as well; that tag stays, with or
	// without the note.
	t.Run("the note does not outweigh another writer of the tag", func(t *testing.T) {
		m := afterRelease(map[string]string{AnnotationOperatorWroteImageTag: "5.1.0"})
		m.ManagedFields = append(m.ManagedFields,
			managedBy("helm", metav1.ManagedFieldsOperationApply, "", `{"f:spec":{"f:image":{"f:tag":{}}}}`))
		result, c := run(t, m)
		if result.Migrated != 0 || len(result.Kept) != 1 || !strings.Contains(result.Kept[0], "helm") || len(c.order) != 0 {
			t.Errorf("got %+v, sent %v", result, c.byName)
		}
	})
	t.Run("nor does it replace an ownership record that is missing", func(t *testing.T) {
		m := afterRelease(map[string]string{AnnotationOperatorWroteImageTag: "5.1.0"})
		m.ManagedFields = nil
		result, c := run(t, m)
		if result.Migrated != 0 || len(result.Kept) != 1 || len(c.order) != 0 {
			t.Errorf("got %+v, sent %v", result, c.byName)
		}
	})
	// Written by hand, or about a tag of long ago: it is about the tag, and
	// goes when the tag does.
	t.Run("a note of any value goes with the tag", func(t *testing.T) {
		m := legacyMonitor("db")
		m.ResourceVersion = "7"
		m.Annotations = map[string]string{AnnotationOperatorWroteImageTag: "4.9.0"}
		result, c := run(t, m)
		if result.Migrated != 1 {
			t.Fatalf("got %+v", result)
		}
		ops := c.byName["db"]
		if last := ops[len(ops)-1]; last["op"] != "remove" || last["path"] != noteAnnotationPath {
			t.Errorf("sent %v, want the note removed as well", ops)
		}
	})
	t.Run("a monitor that never had the note is patched without touching annotations", func(t *testing.T) {
		m := legacyMonitor("db")
		m.ResourceVersion = "7"
		_, c := run(t, m)
		for _, sent := range c.byName["db"] {
			if strings.HasPrefix(sent["path"].(string), "/metadata/annotations") {
				t.Errorf("sent %v", sent)
			}
		}
	})
}

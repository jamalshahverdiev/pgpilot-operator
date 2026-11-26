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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
)

const (
	// finalizerName is the finalizer operator releases before v1.1 put on
	// every monitor. This release no longer adds it and takes it off the
	// monitors that still carry it.
	finalizerName = "pgpilot.io/finalizer"

	// AnnotationOperatorWroteImageTag records, on a monitor whose finalizer
	// was taken off, that the image tag named in its value was written into
	// the spec by an earlier operator release and not by a user.
	//
	// The image migration tells the two apart by the ownership record of the
	// API server: the old operator owns the tag in the same entry as its
	// finalizer. Taking the finalizer off removes that half of the record,
	// so what it proved is written down first. The annotation only means
	// something while the spec still holds that very tag.
	AnnotationOperatorWroteImageTag = "pgpilot.io/operator-wrote-image-tag"
)

// FinalizerRelease takes the operator's finalizer off the monitors that an
// earlier release put it on.
//
// The finalizer never protected anything: everything generated for a monitor
// lives in the monitor's namespace and is owned by it, so the garbage
// collector removes it without help. What the finalizer did do was make every
// deletion wait for a running operator. A monitor, its namespace or the CRD
// could not be deleted while the operator was uninstalled, scaled to zero or
// without its permissions; `helm uninstall` before the monitors were gone
// left them in Terminating for good.
//
// The monitors are read from the API server, with their ownership record,
// and changed with a patch of metadata alone. It runs once each time the
// operator starts leading, after the image migration. A monitor that is being
// deleted is left to the reconciler, which releases it at once.
type FinalizerRelease struct {
	// Reader has to read from the API server: the manager's cache drops
	// managedFields.
	Reader client.Reader
	Client client.Client

	// Backoff paces the retries of a pass that failed. The zero value means
	// defaultImageMigrationBackoff.
	Backoff wait.Backoff
	// PageSize is how many monitors are listed per request; zero means 200.
	PageSize int64
}

// NeedLeaderElection keeps replicas that are not leading from writing.
func (f *FinalizerRelease) NeedLeaderElection() bool { return true }

// Start runs passes until one completes without an error. It never returns an
// error: a monitor that keeps its finalizer is deleted as before, which is no
// reason to stop the operator.
func (f *FinalizerRelease) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("finalizer-release")

	backoff := f.Backoff
	if backoff.Steps == 0 {
		backoff = defaultImageMigrationBackoff
	}
	var lastErr error
	err := wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		released, err := f.Run(ctx)
		if released > 0 {
			log.Info("took the finalizer of earlier operator releases off monitors",
				"monitors", released, "finalizer", finalizerName)
		}
		if err != nil {
			var failed int
			failed, lastErr = firstFailures(err)
			log.Error(lastErr, "could not take the finalizer off every monitor, will retry", "failures", failed)
			return false, nil
		}
		return true, nil
	})
	if err != nil && ctx.Err() == nil {
		log.Error(errors.Join(err, lastErr),
			"giving up for now. Monitors that still carry the finalizer are looked at again when the operator "+
				"next starts; until then they are deleted as before, by the running operator. A monitor the API "+
				"server refuses to change as invalid has to have its spec corrected first")
	}
	return nil
}

// firstFailures bounds what a failed pass puts into the log: it returns how
// many failures err joins and an error that names the first keptInLog of
// them. One line per monitor, at every attempt, is more than a log is for.
func firstFailures(err error) (int, error) {
	var joined interface{ Unwrap() []error }
	if !errors.As(err, &joined) {
		return 1, err
	}
	all := joined.Unwrap()
	if len(all) <= keptInLog {
		return len(all), err
	}
	return len(all), errors.Join(all[:keptInLog]...)
}

// Run makes one pass over all monitors and returns how many it released. An
// error means at least one monitor could not be read or patched; the others
// have been dealt with all the same.
func (f *FinalizerRelease) Run(ctx context.Context) (int, error) {
	var (
		released int
		errs     []error
		opts     = &client.ListOptions{Limit: f.PageSize}
	)
	if opts.Limit <= 0 {
		opts.Limit = defaultImageMigrationPageSize
	}
	for {
		var page pgpilotv1.PgpilotMonitorList
		if err := f.Reader.List(ctx, &page, opts); err != nil {
			return released, errors.Join(append(errs, fmt.Errorf("list monitors: %w", err))...)
		}
		for i := range page.Items {
			monitor := &page.Items[i]
			done, err := f.release(ctx, monitor)
			switch {
			case err != nil:
				errs = append(errs, fmt.Errorf("%s/%s: %w", monitor.Namespace, monitor.Name, err))
			case done:
				released++
			}
		}
		if page.Continue == "" {
			return released, errors.Join(errs...)
		}
		opts.Continue = page.Continue
	}
}

// release takes the finalizer off one monitor.
func (f *FinalizerRelease) release(ctx context.Context, monitor *pgpilotv1.PgpilotMonitor) (bool, error) {
	index := slices.Index(monitor.Finalizers, finalizerName)
	if index < 0 || !monitor.DeletionTimestamp.IsZero() {
		return false, nil
	}

	note, err := tagNote(monitor)
	if err != nil {
		return false, err
	}
	// A JSON patch removes a list entry by its position. The test makes the
	// patch fail when the list changed since it was read and the position is
	// no longer the finalizer's; the next pass reads it again.
	//
	// The version of the object is deliberately not tested: the reconciler
	// writes the status of every monitor while this runs, most of all right
	// after an upgrade, and each of those writes would fail the patch for a
	// change that does not concern it.
	position := fmt.Sprintf("/metadata/finalizers/%d", index)
	ops := append([]jsonPatchOp{
		{Op: jsonPatchTest, Path: position, Value: finalizerName},
		{Op: jsonPatchRemove, Path: position},
	}, note...)

	patch, err := json.Marshal(ops)
	if err != nil {
		return false, err
	}
	if err := f.Client.Patch(ctx, monitor, client.RawPatch(types.JSONPatchType, patch)); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("remove the finalizer: %w", err)
	}
	return true, nil
}

// tagNote returns the patch operations that write
// AnnotationOperatorWroteImageTag, when the monitor still stores the image
// tag of the earlier releases and the ownership record shows that the
// operator wrote it. None when there is nothing to note or it is noted
// already.
func tagNote(monitor *pgpilotv1.PgpilotMonitor) ([]jsonPatchOp, error) {
	if monitor.Spec.Image.Tag != legacyImageTag ||
		monitor.Annotations[AnnotationOperatorWroteImageTag] == legacyImageTag {
		return nil, nil
	}
	owned, err := legacyOwnedImageFields(monitor)
	if err != nil {
		return nil, err
	}
	if !owned.writtenByOperator {
		return nil, nil
	}
	// The note is about this tag and no other: should the tag have been
	// changed since the monitor was read, nothing is written.
	stillThatTag := jsonPatchOp{Op: jsonPatchTest, Path: imageTagPath, Value: legacyImageTag}
	// A JSON patch cannot add a key to a map that does not exist, and adding
	// the map replaces one that does. The monitor was read a moment ago:
	// annotations it was given since must not be lost, so the map is only
	// added while there is still none. The null has to be spelled out; some
	// API servers fail on a test without a value.
	if monitor.Annotations == nil {
		return []jsonPatchOp{
			stillThatTag,
			{Op: jsonPatchTest, Path: annotationsPath, Value: json.RawMessage("null")},
			{
				Op: jsonPatchAdd, Path: annotationsPath,
				Value: map[string]string{AnnotationOperatorWroteImageTag: legacyImageTag},
			},
		}, nil
	}
	return []jsonPatchOp{stillThatTag, {
		Op: jsonPatchAdd, Path: tagNotePath,
		Value: legacyImageTag,
	}}, nil
}

const annotationsPath = "/metadata/annotations"

// tagNotePath is where AnnotationOperatorWroteImageTag sits in a monitor, as
// a JSON pointer.
var tagNotePath = annotationsPath + "/" + jsonPointerToken(AnnotationOperatorWroteImageTag)

// jsonPointerToken escapes a map key for use in a JSON pointer (RFC 6901).
func jsonPointerToken(key string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(key)
}

// legacyMigrations runs, in this order, what has to be undone on monitors
// stored by earlier operator releases: first the image tag is decided on,
// while the ownership record is complete, then the finalizer is taken off.
type legacyMigrations struct {
	// image is nil when the image migration is turned off.
	image      *StoredImageMigration
	finalizers *FinalizerRelease
}

func (l *legacyMigrations) NeedLeaderElection() bool { return true }

func (l *legacyMigrations) Start(ctx context.Context) error {
	if l.image != nil {
		if err := l.image.Start(ctx); err != nil {
			return err
		}
	}
	return l.finalizers.Start(ctx)
}

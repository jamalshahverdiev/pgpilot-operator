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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
	"github.com/jamalshahverdiev/pgpilot-operator/internal/builder"
)

const (
	// legacyFieldManager is the name under which operator releases up to
	// v1.0.2 wrote to the API server. They did not choose one, so the API
	// server took it from the name of the binary.
	legacyFieldManager = "manager"

	// legacyImageTag is the pgwatch tag those releases left in the spec of
	// every monitor: the CRD defaulted spec.image.tag, and the full update
	// with which they added their finalizer made the API server store it.
	legacyImageTag = "5.1.0"

	// AnnotationKeepImageTag, set to "true" on a monitor, makes the
	// migration leave its image tag alone whatever the ownership record says.
	AnnotationKeepImageTag = "pgpilot.io/keep-image-tag"

	// lastAppliedAnnotation is where `kubectl apply` keeps the manifest it
	// applied last.
	lastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

	// reasonImageDefaultRestored is the event recorded on a migrated monitor.
	reasonImageDefaultRestored = "ImageDefaultRestored"

	defaultImageMigrationPageSize = 200

	// keptInLog bounds how many monitors a log line names.
	keptInLog = 20
)

// defaultImageMigrationBackoff paces the passes of the migration that the
// manager runs: six attempts, the last one about two and a half minutes
// after the first.
var defaultImageMigrationBackoff = wait.Backoff{Duration: 5 * time.Second, Factor: 2, Jitter: 0.1, Steps: 6}

// errTagStillDefaulted means the API server put the tag straight back: the
// CRD it serves is still the old one, with a default for spec.image.tag.
var errTagStillDefaulted = errors.New(
	"the API server stored the image tag again after it was removed: the installed PgpilotMonitor CRD " +
		"still defaults spec.image.tag; install the CRDs of this operator release")

// StoredImageMigration removes the pgwatch image an earlier operator release
// wrote into the spec of the monitors it reconciled.
//
// Up to v1.0.2 the operator added its finalizer with an update of the whole
// object. The update carried an empty spec.image, the CRD of the time filled
// in its defaults, and so every monitor ended up with
// `image: {repository: cybertecpostgresql/pgwatch, tag: "5.1.0"}` in its
// stored spec although nobody had asked for that version. A stored tag wins
// over the operator's default, which would keep all those monitors on pgwatch
// 5.1.0 for good.
//
// A tag the user chose must stay, and the value alone does not tell the two
// apart. What can be told is who wrote the field: the API server keeps that
// record (managedFields). The migration removes a tag only when it still has
// the legacy value, the repository is the default one, and the record shows
// no writer but the old operator: either the old operator owns the field, in
// the same entry as its finalizer, or nobody does (a default the API server
// filled in on its own). It keeps its hands off when
//
//   - any other writer owns the tag (kubectl, Helm, a GitOps tool), also
//     together with the old operator,
//   - the manifest last applied with `kubectl apply` contains a tag,
//   - the monitor carries the annotation pgpilot.io/keep-image-tag: "true",
//   - the monitor uses another repository (a mirror may not carry the newer
//     release),
//   - the object has no ownership record for its spec at all (restored from a
//     backup, for instance), so nothing can be concluded.
//
// One case the record cannot show: a write that sets the tag to the value it
// already has changes nothing, and the API server does not note it. A tag
// "5.1.0" confirmed after the fact with `kubectl patch`, `kubectl edit` or a
// Helm upgrade therefore looks like the old operator's and is removed; the
// annotation above is the way to keep it. The same goes for a monitor created
// by another controller whose binary is also called "manager".
//
// It runs once each time the operator starts leading. Monitors created since
// are never candidates, because neither the CRD nor the operator writes the
// image any more.
type StoredImageMigration struct {
	// Reader has to read from the API server: the manager's cache drops
	// managedFields.
	Reader   client.Reader
	Client   client.Client
	Recorder record.EventRecorder

	// Backoff paces the retries of a pass that failed. The zero value means
	// defaultImageMigrationBackoff.
	Backoff wait.Backoff
	// PageSize is how many monitors are listed per request; zero means 200.
	PageSize int64
}

// MigrationResult is what one pass over the monitors did.
type MigrationResult struct {
	// Migrated counts the monitors whose stored tag was removed.
	Migrated int
	// Kept names the monitors that still carry the legacy tag and were left
	// alone, as "namespace/name (why)".
	Kept []string
}

// NeedLeaderElection keeps replicas that are not leading from writing.
func (m *StoredImageMigration) NeedLeaderElection() bool { return true }

// Start runs the migration until one pass completes without an error. It
// never returns an error: a monitor that cannot be migrated keeps running the
// image it has, which is no reason to stop the operator.
func (m *StoredImageMigration) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("stored-image-migration")

	backoff := m.Backoff
	if backoff.Steps == 0 {
		backoff = defaultImageMigrationBackoff
	}

	var (
		lastErr error
		last    MigrationResult
	)
	err := wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		result, err := m.Run(ctx)
		last = result
		if result.Migrated > 0 {
			log.Info("removed the pgwatch image tag written by an earlier operator release",
				"monitors", result.Migrated, "legacyTag", legacyImageTag, "defaultTag", builder.PgwatchVersion)
		}
		if err != nil {
			lastErr = err
			log.Error(err, "could not migrate every monitor, will retry")
			return false, nil
		}
		return true, nil
	})
	if err != nil && ctx.Err() == nil {
		log.Error(errors.Join(err, lastErr),
			"giving up for now. Monitors left with the old image tag are looked at again when the operator "+
				"next starts; a monitor the API server refuses to change has to have its spec corrected first. "+
				"The tag can also be removed by hand: see 'Collector still runs an old pgwatch' in docs/troubleshooting.md")
	}
	if len(last.Kept) > 0 {
		named := last.Kept
		if len(named) > keptInLog {
			named = named[:keptInLog]
		}
		log.Info("monitors with image tag "+legacyImageTag+" were left as they are and keep running that pgwatch release",
			"count", len(last.Kept), "monitors", strings.Join(named, ", "))
	}
	return nil
}

// Run makes one pass over all monitors. An error means at least one monitor
// could not be read or patched; the others have been dealt with all the same,
// unless the error says the pass is pointless (the old CRD is still served).
func (m *StoredImageMigration) Run(ctx context.Context) (MigrationResult, error) {
	var (
		result MigrationResult
		errs   []error
		opts   = &client.ListOptions{Limit: m.PageSize}
	)
	if opts.Limit <= 0 {
		opts.Limit = defaultImageMigrationPageSize
	}
	for {
		var page pgpilotv1.PgpilotMonitorList
		if err := m.Reader.List(ctx, &page, opts); err != nil {
			return result, errors.Join(append(errs, fmt.Errorf("list monitors: %w", err))...)
		}
		for i := range page.Items {
			monitor := &page.Items[i]
			name := monitor.Namespace + "/" + monitor.Name
			migrated, kept, err := m.migrate(ctx, monitor)
			switch {
			case errors.Is(err, errTagStillDefaulted):
				// True for every other monitor as well.
				return result, errors.Join(append(errs, err)...)
			case err != nil:
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
			case migrated:
				result.Migrated++
			case kept != "":
				result.Kept = append(result.Kept, name+" ("+kept+")")
			}
		}
		if page.Continue == "" {
			return result, errors.Join(errs...)
		}
		opts.Continue = page.Continue
	}
}

// The JSON patch operations the migration uses.
const (
	jsonPatchTest   = "test"
	jsonPatchRemove = "remove"
)

type jsonPatchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value string `json:"value,omitempty"`
}

// migrate handles one monitor. kept is non-empty when the monitor carries the
// legacy tag and is deliberately left alone, and says why.
func (m *StoredImageMigration) migrate(
	ctx context.Context,
	monitor *pgpilotv1.PgpilotMonitor,
) (migrated bool, kept string, err error) {
	if !monitor.DeletionTimestamp.IsZero() || monitor.Spec.Image.Tag != legacyImageTag {
		return false, "", nil
	}
	if monitor.Annotations[AnnotationKeepImageTag] == "true" {
		return false, "annotation " + AnnotationKeepImageTag, nil
	}
	// An empty repository means the default one.
	if repository := monitor.Spec.Image.Repository; repository != "" && repository != builder.DefaultImageRepository {
		return false, "its own image repository", nil
	}
	if lastAppliedSetsTag(monitor) {
		return false, "tag set with kubectl apply", nil
	}
	owned, err := legacyOwnedImageFields(monitor)
	if err != nil {
		return false, "", err
	}
	if !owned.tag {
		return false, "tag set by " + owned.why, nil
	}

	// The tests on the values and on the resourceVersion make the patch fail
	// when anything changed since the list: whoever wrote in between may
	// have taken the field over.
	ops := []jsonPatchOp{
		{Op: jsonPatchTest, Path: "/metadata/resourceVersion", Value: monitor.ResourceVersion},
		{Op: jsonPatchTest, Path: "/spec/image/tag", Value: legacyImageTag},
		{Op: jsonPatchRemove, Path: "/spec/image/tag"},
	}
	if owned.repository && monitor.Spec.Image.Repository != "" {
		ops = append(ops,
			jsonPatchOp{Op: jsonPatchTest, Path: "/spec/image/repository", Value: builder.DefaultImageRepository},
			jsonPatchOp{Op: jsonPatchRemove, Path: "/spec/image/repository"},
		)
	}
	patch, err := json.Marshal(ops)
	if err != nil {
		return false, "", err
	}
	if err := m.Client.Patch(ctx, monitor, client.RawPatch(types.JSONPatchType, patch)); err != nil {
		if apierrors.IsNotFound(err) {
			return false, "", nil
		}
		return false, "", fmt.Errorf("remove the stored image tag: %w", err)
	}
	// The response is the object as stored. A CRD that still defaults the
	// tag has filled it in again, and the monitor is where it was.
	if monitor.Spec.Image.Tag != "" {
		return false, "", errTagStillDefaulted
	}
	if m.Recorder != nil {
		m.Recorder.Eventf(monitor, corev1.EventTypeNormal, reasonImageDefaultRestored,
			"Removed image tag %s, which an earlier operator release had written into the spec; "+
				"the collector now runs the operator's default, %s:%s",
			legacyImageTag, builder.DefaultImageRepository, builder.PgwatchVersion)
	}
	return true, "", nil
}

// lastAppliedSetsTag reports whether the manifest last applied to the monitor
// with `kubectl apply` contains spec.image.tag. Applying a manifest whose tag
// equals the stored one changes no field, so this annotation is the only
// trace such a pin leaves.
func lastAppliedSetsTag(monitor *pgpilotv1.PgpilotMonitor) bool {
	raw, ok := monitor.Annotations[lastAppliedAnnotation]
	if !ok {
		return false
	}
	var applied struct {
		Spec struct {
			Image map[string]any `json:"image"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(raw), &applied); err != nil {
		// Unreadable: assume the cautious answer.
		return true
	}
	_, has := applied.Spec.Image["tag"]
	return has
}

type legacyOwnership struct {
	// tag and repository say whether the field may be removed.
	tag, repository bool
	// why names what stands in the way when tag is false.
	why string
}

// legacyOwnedImageFields reports which of spec.image.tag and
// spec.image.repository nobody but the legacy operator has written.
func legacyOwnedImageFields(monitor *pgpilotv1.PgpilotMonitor) (legacyOwnership, error) {
	var (
		legacyOwnsFinalizer, legacyOwnsTag bool
		othersOwnTag, othersOwnRepo        []string
		specIsOwned                        bool
		finalizerKey                       = `v:"` + finalizerName + `"`
	)
	for _, entry := range monitor.ManagedFields {
		if entry.FieldsV1 == nil || len(entry.FieldsV1.GetRawBytes()) == 0 {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal(entry.FieldsV1.GetRawBytes(), &fields); err != nil {
			return legacyOwnership{}, fmt.Errorf("read managedFields of manager %q: %w", entry.Manager, err)
		}
		specIsOwned = specIsOwned || hasFieldPath(fields, "f:spec")
		ownsTag := hasFieldPath(fields, "f:spec", "f:image", "f:tag")
		ownsRepo := hasFieldPath(fields, "f:spec", "f:image", "f:repository")

		legacy := entry.Manager == legacyFieldManager &&
			entry.Operation == metav1.ManagedFieldsOperationUpdate && entry.Subresource == ""
		if legacy {
			legacyOwnsTag = legacyOwnsTag || ownsTag
			legacyOwnsFinalizer = legacyOwnsFinalizer ||
				hasFieldPath(fields, "f:metadata", "f:finalizers", finalizerKey)
			continue
		}
		if ownsTag {
			othersOwnTag = append(othersOwnTag, entry.Manager)
		}
		if ownsRepo {
			othersOwnRepo = append(othersOwnRepo, entry.Manager)
		}
	}

	switch {
	case len(othersOwnTag) > 0:
		return legacyOwnership{why: strings.Join(othersOwnTag, ", ")}, nil
	case legacyOwnsTag && !legacyOwnsFinalizer:
		return legacyOwnership{why: `a writer called "manager" that is not the operator`}, nil
	case !legacyOwnsTag && !specIsOwned:
		// Nobody owns the tag, but nobody owns anything: the record is
		// gone, not telling.
		return legacyOwnership{why: "an unknown writer (the object has no ownership record)"}, nil
	}
	// Owned by the old operator, or by nobody: a default the API server
	// filled in, for example on a status update under the old CRD.
	return legacyOwnership{tag: true, repository: len(othersOwnRepo) == 0}, nil
}

// hasFieldPath reports whether a managedFields tree contains the given path.
func hasFieldPath(fields map[string]any, path ...string) bool {
	var current any = fields
	for _, key := range path {
		node, ok := current.(map[string]any)
		if !ok {
			return false
		}
		if current, ok = node[key]; !ok {
			return false
		}
	}
	return true
}

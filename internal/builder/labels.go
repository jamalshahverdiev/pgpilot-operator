package builder

import (
	"errors"
	"fmt"
	"maps"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	LabelAppName      = "app.kubernetes.io/name"
	LabelAppInstance  = "app.kubernetes.io/instance"
	LabelAppManagedBy = "app.kubernetes.io/managed-by"
	LabelAppComponent = "app.kubernetes.io/component"
	LabelMonitorName  = "pgpilot.io/monitor"

	AnnotationConfigHash = "pgpilot.io/config-hash"
	// AnnotationCredentialsVersion changes whenever the credentials Secret of
	// a monitor changes.
	AnnotationCredentialsVersion = "pgpilot.io/credentials-version"

	AppName   = "pgpilot-monitor"
	ManagedBy = "pgpilot-operator"
)

func SystemLabels(monitorName string) map[string]string {
	return map[string]string{
		LabelAppName:      AppName,
		LabelAppInstance:  monitorName,
		LabelAppManagedBy: ManagedBy,
		LabelAppComponent: "pgwatch",
		LabelMonitorName:  monitorName,
	}
}

func MergeLabels(system, user map[string]string) map[string]string {
	merged := make(map[string]string, len(system)+len(user))
	maps.Copy(merged, user)
	maps.Copy(merged, system) // system keys always win
	return merged
}

func MergeAnnotations(system, user map[string]string) map[string]string {
	merged := make(map[string]string, len(system)+len(user))
	maps.Copy(merged, user)
	maps.Copy(merged, system)
	return merged
}

func ResourceName(monitorName string) string {
	return fmt.Sprintf("pgpilot-%s", monitorName)
}

// errInvalidName is wrapped by the error ValidateMonitorName returns.
var errInvalidName = errors.New("invalid monitor name")

// maxMonitorNameLength is what the generated names leave of the 63
// characters of a Service name.
var maxMonitorNameLength = validation.DNS1035LabelMaxLength - len(ResourceName(""))

// ValidateMonitorName reports whether the objects generated for a monitor of
// this name can exist.
//
// A PgpilotMonitor may be called anything a Kubernetes object may be called:
// up to 253 characters, dots included. Its Service is named pgpilot-<name>,
// and a Service name is a DNS label: at most 63 characters, no dots. The
// name also becomes a label value on everything generated, with the same
// limit. A monitor with a dot in its name, or with 56 to 63 characters, used
// to get a ConfigMap and a Deployment and then fail on the Service for ever,
// with nothing in its status; with a longer name nothing could be created at
// all.
//
// The CRD cannot carry this rule: metadata.name is immutable, so an existing
// monitor that broke it could never be made valid, and on older Kubernetes
// versions could not even be deleted.
func ValidateMonitorName(name string) error {
	if problems := validation.IsDNS1035Label(ResourceName(name)); len(problems) > 0 {
		return fmt.Errorf("%w: %q cannot be used, because the Service generated for it would be called %q, "+
			"which is not a valid Service name (%s). Use a name of at most %d characters made of "+
			"lower-case letters, digits and '-', ending with a letter or digit; "+
			"a monitor cannot be renamed, so create a new one and delete this one",
			errInvalidName, name, ResourceName(name), strings.Join(problems, "; "), maxMonitorNameLength)
	}
	return nil
}

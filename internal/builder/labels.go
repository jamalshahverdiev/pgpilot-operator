package builder

import (
	"fmt"
	"maps"
)

const (
	LabelAppName      = "app.kubernetes.io/name"
	LabelAppInstance  = "app.kubernetes.io/instance"
	LabelAppManagedBy = "app.kubernetes.io/managed-by"
	LabelAppComponent = "app.kubernetes.io/component"
	LabelMonitorName  = "pgpilot.io/monitor"

	AnnotationConfigHash = "pgpilot.io/config-hash"

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

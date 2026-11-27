package builder

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

func TestValidateMonitorName(t *testing.T) {
	valid := []string{
		"db", "my-db", "db1", "1db", "a", strings.Repeat("a", 55),
		"orders-eu-west-1-primary",
	}
	for _, name := range valid {
		if err := ValidateMonitorName(name); err != nil {
			t.Errorf("%q must be valid, got %v", name, err)
		}
		// What the rule is for: every generated name and label value fits.
		// The Deployment, the Service and the ServiceMonitor carry the
		// resource name, the ConfigMap that name with a suffix, and every
		// generated object the monitor's name as a label value.
		if problems := validation.IsDNS1035Label(ResourceName(name)); len(problems) > 0 {
			t.Errorf("%q gives the Service name %q: %v", name, ResourceName(name), problems)
		}
		configMap := ConfigMapName(name, strings.Repeat("f", 64))
		if problems := validation.IsDNS1123Subdomain(configMap); len(problems) > 0 {
			t.Errorf("%q gives the ConfigMap name %q: %v", name, configMap, problems)
		}
		for key, value := range SystemLabels(name) {
			if problems := validation.IsValidLabelValue(value); len(problems) > 0 {
				t.Errorf("%q gives the label %s=%q: %v", name, key, value, problems)
			}
		}
	}

	invalid := map[string]string{
		"a dot":           "db.prod",
		"56 characters":   strings.Repeat("a", 56),
		"253 characters":  strings.Repeat("a", 253),
		"trailing hyphen": "db-",
		"upper case":      "MyDB",
		"underscore":      "my_db",
		"empty":           "",
	}
	for what, name := range invalid {
		err := ValidateMonitorName(name)
		if !errors.Is(err, errInvalidName) {
			t.Errorf("%s (%q): want an invalid name, got %v", what, name, err)
			continue
		}
		// The message is all the user gets: the name that does not work,
		// the Service name that is the reason, and what would work.
		for _, part := range []string{strconv.Quote(name), strconv.Quote("pgpilot-" + name), "at most 55 characters", "cannot be renamed"} {
			if !strings.Contains(err.Error(), part) {
				t.Errorf("%s: message %q does not contain %q", what, err, part)
			}
		}
	}
}

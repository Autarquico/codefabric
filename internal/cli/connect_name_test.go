package cli

import (
	"strings"
	"testing"
)

func TestMachineName_FitsTheControllerLimit(t *testing.T) {
	long := strings.Repeat("a", 80)
	for hostname, want := range map[string]string{
		"laptop":               "laptop",
		"Mac-1760116234.local": "Mac-1760116234",
		long + ".example.com":  strings.Repeat("a", 64),
		"":                     "machine",
		".local":               "machine",
	} {
		if got := machineName(hostname); got != want {
			t.Errorf("machineName(%q) = %q, want %q", hostname, got, want)
		}
	}
}

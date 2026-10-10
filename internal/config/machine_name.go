package config

import "strings"

// MaxMachineName mirrors the Controller's limit on identity names.
const MaxMachineName = 64

// MachineName derives a default identity name from the hostname: its first
// label (CI runners and some laptops report long FQDNs), cut to the limit.
// An empty label yields fallback.
func MachineName(hostname, fallback string) string {
	label, _, _ := strings.Cut(hostname, ".")
	if runes := []rune(label); len(runes) > MaxMachineName {
		label = string(runes[:MaxMachineName])
	}
	if label == "" {
		return fallback
	}
	return label
}

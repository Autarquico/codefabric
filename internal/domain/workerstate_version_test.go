package domain

import "testing"

func TestWorkerVersionCompatible(t *testing.T) {
	tests := []struct {
		name               string
		controller, worker string
		want               bool
	}{
		{"same version", "v0.3.1", "v0.3.1", true},
		{"N: another patch", "v0.3.1", "v0.3.9", true},
		{"N-1", "v0.3.1", "v0.2.5", true},
		{"N-2", "v0.3.1", "v0.1.0", false},
		{"newer than the Controller", "v0.3.1", "v0.4.0", false},
		{"another major", "v1.3.0", "v0.3.0", false},
		{"N-1 does not cross a major", "v1.0.0", "v0.9.0", false},
		{"same major, same minor", "v1.2.0", "v1.2.7", true},
		{"pre-release suffix", "v0.3.0", "v0.3.0-rc.1", true},
		{"build suffix", "v0.3.0", "v0.2.0+abc", true},
		{"no leading v", "0.3.0", "v0.2.0", true},
		{"dev with a dev Controller", "dev", "dev", true},
		{"empty with a dev Controller", "dev", "", false},
		{"not semver with a dev Controller", "dev", "banana", false},
		{"dev with a released Controller", "v0.3.0", "dev", false},
		{"empty with a released Controller", "v0.3.0", "", false},
		{"not semver with a released Controller", "v0.3.0", "banana", false},
		{"released Worker with a dev Controller", "dev", "v0.3.0", false},
		{"the same commit hash on both sides", "3f2a9c1", "3f2a9c1", true},
		{"two different commit hashes", "3f2a9c1", "8b7e0d4", false},
		{"a commit hash Controller with a released Worker", "3f2a9c1", "v0.3.0", false},
		{"a released Controller with a commit hash Worker", "v0.3.0", "3f2a9c1", false},
		{"two components only", "v0.3.0", "v0.3", false},
		{"a number too big for an int", "v0.3.0", "v0.99999999999999999999.0", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := WorkerVersionCompatible(tt.controller, tt.worker); got != tt.want {
				t.Errorf("WorkerVersionCompatible(%q, %q) = %t, want %t", tt.controller, tt.worker, got, tt.want)
			}
		})
	}
}

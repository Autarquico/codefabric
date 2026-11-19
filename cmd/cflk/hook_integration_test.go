//go:build integration && (linux || darwin)

package main

import "os"

// testRelayHook makes the hook of the relay's repositories run as this very test
// binary behaving as `cf`.
func testRelayHook() []string {
	self, err := os.Executable()
	if err != nil {
		return nil
	}
	return []string{"env", asCF + "=1", self, "relay", "hook", "pre-receive"}
}

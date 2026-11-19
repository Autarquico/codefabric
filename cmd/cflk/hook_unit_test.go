//go:build !(integration && (linux || darwin))

package main

// testRelayHook: without the integration tag no test pushes to a relay, and the
// Controller writes its default hook.
func testRelayHook() []string { return nil }

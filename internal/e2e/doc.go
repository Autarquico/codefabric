// Package e2e runs the compiled cf binary as real processes (a Controller and
// its Workers) and checks the invariants of the system from the outside. The
// tests live behind the e2e build tag; netproxy, the TCP proxy that partitions
// a Worker from its Controller, is a plain package tested on every run.
package e2e

//go:build integration

package gitops

// TrustCA makes the authenticated fetches of g trust the CA in the PEM file
// path, which the servers of the tests are signed by. It exists only in
// builds with the integration tag, which are never shipped.
func TrustCA(g *Git, path string) { g.trustCA = path }

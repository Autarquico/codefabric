package gitops

// TrustCA makes the authenticated fetches of g trust the CA in the PEM file
// path, which the servers of the tests are signed by.
func TrustCA(g *Git, path string) { g.trustCA = path }

// OriginEnv is the environment of an authenticated fetch of the repository bare.
func OriginEnv(g *Git, bare string, a *OriginAuth) ([]string, error) {
	f, err := g.originFetch(bare, a)
	return f.env, err
}

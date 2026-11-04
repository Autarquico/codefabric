package gitops

// OriginEnv is the environment of an authenticated fetch of the repository bare.
func OriginEnv(g *Git, bare string, a *OriginAuth) ([]string, error) {
	f, err := g.originFetch(bare, a)
	return f.env, err
}

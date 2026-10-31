package domain

// harnessRequiredSecrets maps a harness to the secrets every Run of it needs
// (§16.7). The Controller cannot import adapters (ADR 0001), so it reads this
// mapping; each adapter asserts in a test that its harness.SecretRequirer
// declares the same list.
var harnessRequiredSecrets = map[string][]SecretEnv{
	"claude-code": {{Name: "CLAUDE_CODE_OAUTH_TOKEN", Ref: "claude/oauth-token"}},
}

// HarnessRequiredSecrets is the secrets a Run of the harness always uses, or
// nil when it needs none. The slice is a copy.
func HarnessRequiredSecrets(harness string) []SecretEnv {
	return append([]SecretEnv(nil), harnessRequiredSecrets[harness]...)
}

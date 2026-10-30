package execwrap

import (
	"bytes"
	"fmt"
	"regexp"

	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/redact"
)

// envName is the name of an environment variable a secret may take: no '=',
// nothing a shell would not take as a name.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// CheckSecretNames refuses a secret that goes to something that is not the name
// of a variable.
func CheckSecretNames(secrets []SecretEnv) error {
	for _, s := range secrets {
		if !envName.MatchString(s.Name) {
			return fmt.Errorf("%s: the secret %q goes to %q, which is not the name of a variable", domain.CodeSecretUnavailable, s.Ref, s.Name)
		}
	}
	return nil
}

// Redaction is the matcher of everything the output of the Run is redacted of.
func Redaction(load LoadSecrets) (*redact.Set, error) {
	set, err := redaction(load)
	if err != nil {
		return nil, fmt.Errorf("%s: the output cannot be redacted: %w", domain.CodeSecretUnavailable, err)
	}
	return set, nil
}

// ResolveSecrets is the NAME=value environment of secrets, resolved now. It
// refuses a value that set would not redact from the output.
func ResolveSecrets(secrets []SecretEnv, resolve ResolveSecret, set *redact.Set) ([]string, error) {
	var env []string
	for _, s := range secrets {
		if resolve == nil {
			return nil, fmt.Errorf("%s: the secret %q of %s cannot be resolved: this Worker has no secret store", domain.CodeSecretUnavailable, s.Ref, s.Name)
		}
		value, err := resolve(s.Ref)
		if err != nil {
			return nil, fmt.Errorf("%s: the secret %q of %s cannot be resolved: %w", domain.CodeSecretUnavailable, s.Ref, s.Name, err)
		}
		if bytes.Contains(redact.Bytes([]byte(value), set), []byte(value)) {
			return nil, fmt.Errorf("%s: the secret %q of %s would not be redacted from the output", domain.CodeSecretUnavailable, s.Ref, s.Name)
		}
		env = append(env, s.Name+"="+value)
	}
	return env, nil
}

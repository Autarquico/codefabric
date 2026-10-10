package v1

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

const fingerprintPrefix = "sha256:"

// Fingerprint is the pin of a CA: sha256 of its DER encoding, as lowercase hex.
func Fingerprint(caDER []byte) string {
	sum := sha256.Sum256(caDER)
	return hex.EncodeToString(sum[:])
}

// Invite is a parsed `cf://<host>:<port>/join?token=<t>#sha256:<fingerprint>`.
type Invite struct {
	Host        string
	Port        string
	Token       string
	Fingerprint string // lowercase hex of the CA's sha256
}

// Address is host:port, ready to dial.
func (i Invite) Address() string { return net.JoinHostPort(i.Host, i.Port) }

// String renders the invitation.
func (i Invite) String() string {
	u := url.URL{
		Scheme:   "cf",
		Host:     i.Address(),
		Path:     "/join",
		RawQuery: url.Values{"token": {i.Token}}.Encode(),
		Fragment: fingerprintPrefix + i.Fingerprint,
	}
	return u.String()
}

// ErrInvalidInvite is wrapped by every ParseInvite failure.
var ErrInvalidInvite = errors.New("invalid invitation")

// ParseInvite validates the whole shape: a token without a pinned fingerprint
// would turn the join into trust-on-first-use, so it is refused.
func ParseInvite(s string) (Invite, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return Invite{}, fmt.Errorf("%w: not a URL", ErrInvalidInvite)
	}
	if u.Scheme != "cf" || u.Path != "/join" {
		return Invite{}, fmt.Errorf("%w: expected cf://<host>:<port>/join?token=<token>", ErrInvalidInvite)
	}
	port := u.Port()
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 || u.Hostname() == "" {
		return Invite{}, fmt.Errorf("%w: missing host or port", ErrInvalidInvite)
	}
	token := u.Query().Get("token")
	if token == "" {
		return Invite{}, fmt.Errorf("%w: missing token", ErrInvalidInvite)
	}
	fp, ok := strings.CutPrefix(u.Fragment, fingerprintPrefix)
	if raw, err := hex.DecodeString(fp); !ok || err != nil || len(raw) != sha256.Size {
		return Invite{}, fmt.Errorf("%w: missing or malformed #sha256:<fingerprint>", ErrInvalidInvite)
	}
	return Invite{Host: u.Hostname(), Port: port, Token: token, Fingerprint: strings.ToLower(fp)}, nil
}

// Redact hides the token of an invitation for display in config and logs.
func (i Invite) Redact() string {
	i.Token = "REDACTED"
	return i.String()
}

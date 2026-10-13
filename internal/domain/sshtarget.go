package domain

import (
	"errors"
	"net/netip"
	"strconv"
	"strings"
)

// maxSSHTargetLen bounds what a Worker may announce as the way to reach it.
const maxSSHTargetLen = 255

// SSHTarget is where a Worker can be reached with ssh: [user@]host[:port]. The
// text comes from a Worker, and a client turns it into a local process, so it is
// parsed into parts that cannot be read as an option, a command or a second
// destination: names are plain words and an IPv6 address is a netip.Addr.
type SSHTarget struct {
	User string
	Host string // a name, an IPv4 address or, between brackets in the text, an IPv6 one
	Port int    // 0 when the target names none
}

// ErrMalformedSSHTarget is returned for text that is not an SSH target.
var ErrMalformedSSHTarget = errors.New("malformed ssh target")

// ParseSSHTarget reads (USER@)?HOST(:PORT)?. USER and HOST are made of letters,
// digits, '.', '_' and '-', and cannot begin with '-' (ssh would read an option)
// or '.'. An IPv6 address goes between brackets and has no zone. The port is
// between 1 and 65535. The text is at most 255 bytes.
func ParseSSHTarget(s string) (SSHTarget, error) {
	var t SSHTarget
	bad := func(why string) (SSHTarget, error) {
		return SSHTarget{}, errors.Join(ErrMalformedSSHTarget, errors.New(why))
	}
	if s == "" || len(s) > maxSSHTargetLen {
		return bad("empty or longer than 255 bytes")
	}
	rest := s
	if user, host, found := strings.Cut(rest, "@"); found {
		if !sshWord(user) {
			return bad("the user is not a plain word")
		}
		t.User, rest = user, host
	}
	if strings.HasPrefix(rest, "[") {
		addrText, after, found := strings.Cut(rest[1:], "]")
		if !found {
			return bad("unterminated IPv6 address")
		}
		addr, err := netip.ParseAddr(addrText)
		if err != nil || !addr.Is6() || addr.Zone() != "" {
			return bad("not an IPv6 address without zone")
		}
		t.Host, rest = addr.String(), after
		if rest != "" {
			port, found := strings.CutPrefix(rest, ":")
			if !found {
				return bad("text after the IPv6 address")
			}
			return t.withPort(port, bad)
		}
		return t, nil
	}
	host, port, hasPort := strings.Cut(rest, ":")
	if !sshWord(host) {
		return bad("the host is not a plain word")
	}
	t.Host = host
	if !hasPort {
		return t, nil
	}
	return t.withPort(port, bad)
}

func (t SSHTarget) withPort(port string, bad func(string) (SSHTarget, error)) (SSHTarget, error) {
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n < 1 || strconv.FormatUint(n, 10) != port {
		return bad("the port is not a number between 1 and 65535")
	}
	t.Port = int(n)
	return t, nil
}

// sshWord: one or more of letters, digits, '.', '_' and '-', not beginning with
// '-' or '.'.
func sshWord(s string) bool {
	if s == "" || s[0] == '-' || s[0] == '.' {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// Destination is the argument ssh takes for the machine: user@host, or
// user@[v6] for an IPv6 address.
func (t SSHTarget) Destination() string {
	host := t.Host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if t.User == "" {
		return host
	}
	return t.User + "@" + host
}

// ValidSSHTarget reports whether s is an SSH target.
func ValidSSHTarget(s string) bool {
	_, err := ParseSSHTarget(s)
	return err == nil
}

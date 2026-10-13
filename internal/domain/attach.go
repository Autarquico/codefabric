package domain

import "path"

const (
	maxAttachBinaries   = 4
	maxAttachBinaryPath = 255
	maxAttachRuntimeLen = 32
)

// ValidAttachRuntime reports whether name can key the binaries a Worker offers
// for attaching: lower case letters, digits and '-', up to 32.
func ValidAttachRuntime(name string) bool {
	if name == "" || len(name) > maxAttachRuntimeLen {
		return false
	}
	for i := range len(name) {
		c := name[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// ValidAttachBinary reports whether p can be the program a client runs on a
// Worker to attach: an absolute, clean path of letters, digits and . _ + / -,
// of at most 255 bytes, with no '.' or '..' components (so it is its own clean form), and not the root. It is text from a
// Worker that ends in a command line, so nothing else is accepted.
func ValidAttachBinary(p string) bool {
	if len(p) < 2 || len(p) > maxAttachBinaryPath || p[0] != '/' || path.Clean(p) != p {
		return false
	}
	for i := range len(p) {
		c := p[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '+', c == '/', c == '-':
		default:
			return false
		}
	}
	return true
}

// ValidAttachBinaries checks the whole map a Worker announces: at most four
// entries, each a valid runtime name and binary.
func ValidAttachBinaries(m map[string]string) bool {
	if len(m) > maxAttachBinaries {
		return false
	}
	for runtime, bin := range m {
		if !ValidAttachRuntime(runtime) || !ValidAttachBinary(bin) {
			return false
		}
	}
	return true
}

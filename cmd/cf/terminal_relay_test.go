package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeysBefore_TheDetachKeyIsTheOneGiven(t *testing.T) {
	for _, tc := range []struct {
		name  string
		typed string
		key   byte
		keys  string
		leave bool
	}{
		{"the default", "ls\x1d", 0x1d, "ls", true},
		{"nothing typed before it", "\x1d", 0x1d, "", true},
		{"a configured key", "ab\x07cd", 0x07, "ab", true},
		{"the old key is just a key now", "ab\x1dcd", 0x07, "ab\x1dcd", false},
		{"no key", "plain", 0x1d, "plain", false},
		{"Ctrl-] in CSI-u", "ls\x1b[93;5u", 0x1d, "ls", true},
		{"Ctrl-G in CSI-u", "\x1b[103;5u", 0x07, "", true},
		{"a press in CSI-u", "a\x1b[93;5:1uz", 0x1d, "a", true},
		{"a release is not a press", "\x1b[93;5:3u", 0x1d, "\x1b[93;5:3u", false},
		{"another key in CSI-u", "\x1b[103;5u", 0x1d, "\x1b[103;5u", false},
	} {
		keys, leave := keysBefore([]byte(tc.typed), tc.key)
		if !bytes.Equal(keys, []byte(tc.keys)) || leave != tc.leave {
			t.Errorf("%s: keysBefore(%q, %#x) = %q, %v; want %q, %v", tc.name, tc.typed, tc.key, keys, leave, tc.keys, tc.leave)
		}
	}
}

// The detach key comes from the client config that cf already reads, and an
// unreadable one is an error and not a silent default.
func TestDetachKeyOf_ComesFromTheClientConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CF_CONFIG_DIR", dir)
	t.Setenv("CF_DATA_DIR", t.TempDir())
	if key, name, err := detachKeyOf(); err != nil || key != 0x1d || name != "Ctrl-]" {
		t.Fatalf("no config: %#x %q %v, want Ctrl-]", key, name, err)
	}
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "client.json"), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"attach":{"detach_key":"ctrl+g"}}`)
	if key, name, err := detachKeyOf(); err != nil || key != 0x07 || name != "Ctrl-G" {
		t.Fatalf("ctrl+g: %#x %q %v", key, name, err)
	}
	write(`{"attach":{"detach_key":"ctrl+c"}}`)
	if _, _, err := detachKeyOf(); err == nil {
		t.Fatal("a detach key that would break the session was accepted")
	} else if !strings.Contains(err.Error(), filepath.Join(dir, "client.json")) {
		t.Errorf("the error does not name the file to fix: %v", err)
	}
}

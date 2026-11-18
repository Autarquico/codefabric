package claude

import (
	"errors"
	"io/fs"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeEntry is a directory, a file or a link of a fake file system.
type fakeEntry struct {
	mode fs.FileMode
	uid  uint32
	to   string // for a link
}

type fakeInfo struct {
	fakeEntry
	name string
}

func (i fakeInfo) Name() string       { return i.name }
func (i fakeInfo) Size() int64        { return 0 }
func (i fakeInfo) Mode() fs.FileMode  { return i.mode }
func (i fakeInfo) ModTime() time.Time { return time.Time{} }
func (i fakeInfo) IsDir() bool        { return i.mode.IsDir() }
func (i fakeInfo) Sys() any           { return &syscall.Stat_t{Uid: i.uid} }

// fakeFS makes a resolver that lives in a file system of entries by path and
// is the user 1000, so that links and directories of "another user" (2000) can
// be planted without a second account.
func fakeFS(entries map[string]fakeEntry) resolver {
	return resolver{
		uid: 1000,
		lstat: func(p string) (fs.FileInfo, error) {
			e, ok := entries[p]
			if !ok {
				return nil, fs.ErrNotExist
			}
			return fakeInfo{e, p}, nil
		},
		readlink: func(p string) (string, error) {
			e, ok := entries[p]
			if !ok || e.mode&fs.ModeSymlink == 0 {
				return "", errors.New("not a link")
			}
			return e.to, nil
		},
	}
}

const (
	me, root, other = 1000, 0, 2000
	dir755          = fs.ModeDir | 0o755
	dir777          = fs.ModeDir | 0o777
	dir700          = fs.ModeDir | 0o700
	link            = fs.ModeSymlink | 0o777
)

func TestResolver(t *testing.T) {
	base := func() map[string]fakeEntry {
		return map[string]fakeEntry{
			"/":                      {dir755, root, ""},
			"/srv":                   {dir755, root, ""},
			"/srv/shared":            {dir777, me, ""}, // others can write it
			"/srv/shared/team":       {dir755, me, ""},
			"/srv/shared/team/tdd":   {dir755, me, ""},
			"/home":                  {dir755, root, ""},
			"/home/me":               {dir700, me, ""},
			"/home/me/private":       {dir700, me, ""},
			"/home/me/private/tdd":   {dir700, me, ""},
			"/home/me/.claude":       {dir700, me, ""},
			"/home/me/.claude/skill": {dir700, me, ""},
		}
	}
	for _, c := range []struct {
		name    string
		mutate  func(map[string]fakeEntry)
		target  string
		want    string // the resolved path
		refuses string // part of the error
	}{
		{
			name:    "a real directory under a parent that others can write is refused",
			target:  "/srv/shared/team/tdd",
			refuses: `"/srv/shared"`,
		},
		{
			name: "a real directory under a parent that the user and root own is followed",
			mutate: func(m map[string]fakeEntry) {
				m["/srv/shared"] = fakeEntry{dir755, root, ""}
			},
			target: "/srv/shared/team/tdd",
			want:   "/srv/shared/team/tdd",
		},
		{
			// SEC-4: another user swaps shared/team for a link into the
			// directories of the victim, which pass every check once resolved.
			name: "an intermediate link planted by another user is refused",
			mutate: func(m map[string]fakeEntry) {
				m["/srv/shared"] = fakeEntry{dir755, root, ""}
				m["/srv/shared/team"] = fakeEntry{link, other, "/home/me/private"}
			},
			target:  "/srv/shared/team/tdd",
			refuses: `the link "/srv/shared/team", which another user owns`,
		},
		{
			name: "an intermediate link of the user is followed",
			mutate: func(m map[string]fakeEntry) {
				m["/srv/shared"] = fakeEntry{dir755, root, ""}
				m["/srv/shared/team"] = fakeEntry{link, me, "/home/me/private"}
			},
			target: "/srv/shared/team/tdd",
			want:   "/home/me/private/tdd",
		},
		{
			name: "an intermediate link of root is followed",
			mutate: func(m map[string]fakeEntry) {
				m["/srv/shared"] = fakeEntry{dir755, root, ""}
				m["/srv/shared/team"] = fakeEntry{link, root, "/home/me/private"}
			},
			target: "/srv/shared/team/tdd",
			want:   "/home/me/private/tdd",
		},
		{
			name: "a relative link is followed from its own directory",
			mutate: func(m map[string]fakeEntry) {
				m["/srv/shared"] = fakeEntry{dir755, root, ""}
				m["/srv/shared/current"] = fakeEntry{link, me, "team"}
			},
			target: "/srv/shared/current/tdd",
			want:   "/srv/shared/team/tdd",
		},
		{
			name: "a link whose holder is a directory others can write is refused before the link",
			mutate: func(m map[string]fakeEntry) {
				m["/srv/shared/team"] = fakeEntry{link, me, "/home/me/private"}
			},
			target:  "/srv/shared/team/tdd",
			refuses: `"/srv/shared"`,
		},
		{
			name: "a directory of another user in the way is refused",
			mutate: func(m map[string]fakeEntry) {
				m["/srv/shared"] = fakeEntry{dir755, root, ""}
				m["/srv/shared/team"] = fakeEntry{dir755, other, ""}
			},
			target:  "/srv/shared/team/tdd",
			refuses: `"/srv/shared/team"`,
		},
		{
			name: "a sticky directory others can write is accepted",
			mutate: func(m map[string]fakeEntry) {
				m["/srv/shared"] = fakeEntry{dir777 | fs.ModeSticky, root, ""}
			},
			target: "/srv/shared/team/tdd",
			want:   "/srv/shared/team/tdd",
		},
		{
			name: "links in a loop are refused",
			mutate: func(m map[string]fakeEntry) {
				m["/srv/a"] = fakeEntry{link, me, "/srv/b"}
				m["/srv/b"] = fakeEntry{link, me, "/srv/a"}
			},
			target:  "/srv/a/x",
			refuses: "too many links",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := base()
			if c.mutate != nil {
				c.mutate(m)
			}
			got, err := fakeFS(m).resolve("skills/tdd", "/home/me/.claude/skills", c.target)
			switch {
			case c.refuses != "":
				if err == nil || !strings.Contains(err.Error(), c.refuses) {
					t.Errorf("= %q, %v; want an error with %s", got, err, c.refuses)
				}
			case err != nil || got != c.want:
				t.Errorf("= %q, %v; want %q", got, err, c.want)
			}
		})
	}
}

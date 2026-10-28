// Package treedigest computes treedigest/v1, a content digest of a directory
// tree that ignores everything but names, structure, file contents and the
// executable bit.
//
// # Tree
//
// A tree is a set of entries, each a file or a directory, named by a path
// relative to the root. A path is valid UTF-8, separated by "/", non-empty,
// with no empty, "." or ".." components, no NUL byte and no leading or
// trailing "/". Paths are compared as raw bytes: no Unicode normalization, so
// "é" and "é" are different names. Every ancestor of an entry is a
// directory of the tree whether or not it is listed, so listing a non-empty
// directory changes nothing and an empty directory counts only because it is
// listed. A tree is invalid if a path appears twice or is both a file and an
// ancestor of another entry.
//
// A file contributes its executable bit (the owner's, mode 0100), its size and
// the SHA-256 of its contents; a directory, only its path. Nothing else counts:
// not times, owners, other permission bits or the order of the input.
//
// # Encoding
//
// With frame(b) = uint64 big-endian len(b) || b, the digest is
//
//	"sha256:" || lowercase hex(SHA-256(stream))
//	stream = frame("treedigest/v1") || record...
//
// with one record per entry, implied directories included, in ascending byte
// order of path:
//
//	directory: 'D' || frame(path)
//	file:      'F' || frame(path) || exec || uint64 big-endian size || SHA-256(contents)
//
// where exec is the byte 0x01 if the file is executable and 0x00 otherwise.
// The empty tree is therefore SHA-256 of frame("treedigest/v1") alone.
//
// # Sources
//
// [Files] digests a tree given as entries and [Dir] one read from disk; both
// build the same tree and share the encoding, so a directory and the entries
// that describe it give the same digest.
package treedigest

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"iter"
	"os"
	"slices"
	"strings"
	"unicode/utf8"
)

// version is the header of every digested stream.
const version = "treedigest/v1"

var (
	// ErrInvalidPath reports a path that breaks the rules of the package doc.
	ErrInvalidPath = errors.New("treedigest: invalid path")
	// ErrDuplicatePath reports a path listed twice.
	ErrDuplicatePath = errors.New("treedigest: duplicate path")
	// ErrCollision reports a path that is a file and an ancestor of another entry.
	ErrCollision = errors.New("treedigest: file used as a directory")
	// ErrUnsupportedType reports a symlink that is not approved, a FIFO, a
	// socket, a device or anything else that is neither a file nor a directory.
	ErrUnsupportedType = errors.New("treedigest: unsupported file type")
)

// Entry is a [File] or a [Directory].
type Entry interface {
	node() node
}

// File is a regular file of the tree.
type File struct {
	Path    string
	Exec    bool
	Content []byte
}

// Directory is a directory of the tree; only empty ones need to be listed.
type Directory struct {
	Path string
}

func (f File) node() node {
	return node{path: f.Path, file: true, exec: f.Exec, size: uint64(len(f.Content)), sum: sha256.Sum256(f.Content)}
}

func (d Directory) node() node { return node{path: d.Path} }

// node is an entry as the encoding sees it.
type node struct {
	path string
	file bool
	exec bool
	size uint64
	sum  [sha256.Size]byte
}

// Files returns the treedigest/v1 of the tree the entries describe, in any order.
func Files(entries []Entry) (string, error) {
	nodes := make([]node, 0, len(entries))
	for i, e := range entries {
		if e == nil {
			return "", fmt.Errorf("treedigest: entry %d is nil", i)
		}
		nodes = append(nodes, e.node())
	}
	return digest(nodes)
}

// Options tunes [Dir].
type Options struct {
	// ApprovedLinks are the paths, relative to the root, of the symlinks to
	// leave out of the digest. Any other symlink is an error, and so is an
	// approved path that exists but is not a symlink; one that does not exist
	// is ignored.
	ApprovedLinks []string
	// ApprovedFiles are the paths, relative to the root, of the regular files
	// to leave out of the digest, because their content is not part of the
	// tree: a copy the program that reads it rewrites. An approved path that
	// exists but is not a regular file is an error; one that does not exist
	// is ignored.
	ApprovedFiles []string
}

// Dir returns the treedigest/v1 of the tree under root, which is not part of
// the tree itself. It never follows symlinks.
func Dir(root string, opts Options) (string, error) {
	approved := make(map[string]bool, len(opts.ApprovedLinks))
	for _, p := range opts.ApprovedLinks {
		if err := checkPath(p); err != nil {
			return "", fmt.Errorf("approved link: %w", err)
		}
		approved[p] = true
	}
	files := make(map[string]bool, len(opts.ApprovedFiles))
	for _, p := range opts.ApprovedFiles {
		if err := checkPath(p); err != nil {
			return "", fmt.Errorf("approved file: %w", err)
		}
		files[p] = true
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return "", fmt.Errorf("treedigest: opening %s: %w", root, err)
	}
	defer func() { _ = r.Close() }()

	var nodes []node
	err = fs.WalkDir(r.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		typ := d.Type()
		if approved[p] {
			if typ&fs.ModeSymlink == 0 {
				return fmt.Errorf("treedigest: approved link %q is not a symlink", p)
			}
			return nil
		}
		if files[p] {
			if !typ.IsRegular() {
				return fmt.Errorf("treedigest: approved file %q is not a regular file", p)
			}
			return nil
		}
		switch {
		case typ.IsDir():
			nodes = append(nodes, node{path: p})
			return nil
		case typ.IsRegular():
			n, err := readFile(r, p)
			if err != nil {
				return err
			}
			nodes = append(nodes, n)
			return nil
		default:
			return fmt.Errorf("%w: %q is %v", ErrUnsupportedType, p, typ)
		}
	})
	if err != nil {
		return "", fmt.Errorf("treedigest: reading %s: %w", root, err)
	}
	return digest(nodes)
}

// readFile hashes the regular file at p, checking it is still one once open.
func readFile(r *os.Root, p string) (node, error) {
	f, err := r.Open(p)
	if err != nil {
		return node{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return node{}, err
	}
	if !info.Mode().IsRegular() {
		return node{}, fmt.Errorf("%w: %q is %v", ErrUnsupportedType, p, info.Mode().Type())
	}
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return node{}, fmt.Errorf("hashing %q: %w", p, err)
	}
	n := node{path: p, file: true, exec: info.Mode().Perm()&0o100 != 0, size: uint64(size)} //nolint:gosec // io.Copy never counts below zero
	h.Sum(n.sum[:0])
	return n, nil
}

// digest validates the tree the nodes describe, completes it with the implied
// directories and encodes it.
func digest(nodes []node) (string, error) {
	tree := make(map[string]node, len(nodes))
	for _, n := range nodes {
		if err := checkPath(n.path); err != nil {
			return "", err
		}
		if _, dup := tree[n.path]; dup {
			return "", fmt.Errorf("%w: %q", ErrDuplicatePath, n.path)
		}
		tree[n.path] = n
	}
	for _, n := range nodes {
		for dir := range ancestors(n.path) {
			if a, ok := tree[dir]; ok {
				if a.file {
					return "", fmt.Errorf("%w: %q is a file and %q is under it", ErrCollision, dir, n.path)
				}
				continue
			}
			tree[dir] = node{path: dir}
		}
	}
	paths := make([]string, 0, len(tree))
	for p := range tree {
		paths = append(paths, p)
	}
	slices.Sort(paths)

	h := sha256.New()
	writeFrame(h, version)
	for _, p := range paths {
		writeRecord(h, tree[p])
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func writeRecord(h hash.Hash, n node) {
	if !n.file {
		h.Write([]byte{'D'})
		writeFrame(h, n.path)
		return
	}
	h.Write([]byte{'F'})
	writeFrame(h, n.path)
	exec := byte(0)
	if n.exec {
		exec = 1
	}
	h.Write([]byte{exec})
	h.Write(binary.BigEndian.AppendUint64(nil, n.size))
	h.Write(n.sum[:])
}

func writeFrame(h hash.Hash, s string) {
	h.Write(binary.BigEndian.AppendUint64(nil, uint64(len(s))))
	h.Write([]byte(s))
}

// ancestors yields the proper ancestors of a valid path, nearest last.
func ancestors(p string) iter.Seq[string] {
	return func(yield func(string) bool) {
		for i := range len(p) {
			if p[i] == '/' && !yield(p[:i]) {
				return
			}
		}
	}
}

func checkPath(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("%w: empty", ErrInvalidPath)
	case !utf8.ValidString(p):
		return fmt.Errorf("%w: %q is not valid UTF-8", ErrInvalidPath, p)
	case strings.ContainsRune(p, 0):
		return fmt.Errorf("%w: %q contains NUL", ErrInvalidPath, p)
	case strings.HasPrefix(p, "/"):
		return fmt.Errorf("%w: %q is absolute", ErrInvalidPath, p)
	}
	for c := range strings.SplitSeq(p, "/") {
		if c == "" || c == "." || c == ".." {
			return fmt.Errorf("%w: %q has an empty, \".\" or \"..\" component", ErrInvalidPath, p)
		}
	}
	return nil
}

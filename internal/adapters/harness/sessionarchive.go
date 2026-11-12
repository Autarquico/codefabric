package harness

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"
)

// The layout every SessionPorter writes: a tar.gz of manifest.json, then
// <id>.jsonl, then the attachments as <id>/<dir>/<rel>.
const sessionManifestName = "manifest.json"

// maxArchiveEntries bounds the files of an archive: the byte limit does not
// count the empty ones.
const maxArchiveEntries = 10_000

// Limit is MaxBytes, or [DefaultMaxSessionBytes] when it is not positive.
func (l SessionLimit) Limit() int64 {
	if l.MaxBytes <= 0 {
		return DefaultMaxSessionBytes
	}
	return l.MaxBytes
}

// CheckSessionSize is [ErrSessionTooLarge] when the files of a session add up
// to more than limit bytes.
func CheckSessionSize(adapter string, files SessionFiles, limit int64) error {
	var total int64
	for _, p := range append([]string{files.Transcript}, files.Attachments...) {
		info, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("%s: sizing the session: %w", adapter, err)
		}
		total += info.Size()
	}
	if total > limit {
		return fmt.Errorf("%w: %d bytes, the limit is %d", ErrSessionTooLarge, total, limit)
	}
	return nil
}

// ArchiveFile is a file of a session archive. Name is relative to <id>/ for an
// attachment.
type ArchiveFile struct {
	Name string
	Data []byte
}

// WriteSessionArchive writes the archive of a session: sorted as given, with
// no owners, times or gzip header, so the same session gives the same bytes.
// Attachment names are relative to <id>/.
func WriteSessionArchive(w io.Writer, manifest SessionManifest, transcript []byte, attachments []ArchiveFile) error {
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	put := func(name string, data []byte) error {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), ModTime: time.Unix(0, 0)}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	if err := put(sessionManifestName, manifestJSON); err != nil {
		return err
	}
	if err := put(manifest.SessionID+".jsonl", transcript); err != nil {
		return err
	}
	for _, f := range attachments {
		if err := put(manifest.SessionID+"/"+f.Name, f.Data); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// SessionArchive is what an archive of [WriteSessionArchive] holds.
type SessionArchive struct {
	Manifest   SessionManifest
	Transcript []byte
	// Attachments have names relative to <id>/, as the writer gave them.
	Attachments []ArchiveFile
}

// ArchiveRules are what ReadSessionArchive accepts of an archive.
type ArchiveRules struct {
	// Adapter is the ID the manifest must carry, and the prefix of errors.
	Adapter string
	// ValidID tells whether the id of the manifest can reach a path.
	ValidID func(string) bool
	// AttachmentDir is the directory under <id>/ attachments may live in;
	// empty accepts none.
	AttachmentDir string
}

// ReadSessionArchive reads an archive into memory, refusing what the writer
// does not write: other names, links and paths that climb.
func ReadSessionArchive(ctx context.Context, r io.Reader, limit int64, rules ArchiveRules) (SessionArchive, error) {
	var arc SessionArchive
	fail := func(format string, args ...any) (SessionArchive, error) {
		return arc, fmt.Errorf(rules.Adapter+": "+format, args...)
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fail("the session archive: %w", err)
	}
	defer gz.Close() //nolint:errcheck // reading
	tr := tar.NewReader(gz)
	var haveManifest, haveTranscript bool
	remaining, entries := limit, 0
	for {
		if err := ctx.Err(); err != nil {
			return arc, err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fail("the session archive: %w", err)
		}
		if entries++; entries > maxArchiveEntries {
			return arc, fmt.Errorf("%w: more than %d entries", ErrSessionTooLarge, maxArchiveEntries)
		}
		if hdr.Typeflag != tar.TypeReg {
			return fail("the session archive has %q, which is not a file", hdr.Name)
		}
		data, err := io.ReadAll(io.LimitReader(tr, remaining+1))
		if err != nil {
			return fail("the session archive: %w", err)
		}
		if remaining -= int64(len(data)); remaining < 0 {
			return arc, fmt.Errorf("%w: the limit is %d bytes", ErrSessionTooLarge, limit)
		}
		switch {
		case hdr.Name == sessionManifestName && !haveManifest:
			if err := json.Unmarshal(data, &arc.Manifest); err != nil {
				return fail("the manifest of the archive: %w", err)
			}
			if arc.Manifest.Adapter != rules.Adapter || !rules.ValidID(arc.Manifest.SessionID) {
				return fail("the archive is not a %s session", rules.Adapter)
			}
			haveManifest = true
		case !haveManifest:
			return fail("the archive does not start with its manifest")
		case hdr.Name == arc.Manifest.SessionID+".jsonl" && !haveTranscript:
			arc.Transcript, haveTranscript = data, true
		default:
			rel, ok := strings.CutPrefix(hdr.Name, arc.Manifest.SessionID+"/"+rules.AttachmentDir+"/")
			if rules.AttachmentDir == "" || !ok || !fs.ValidPath(rel) || rel == "." {
				return fail("the session archive has an unexpected %q", hdr.Name)
			}
			arc.Attachments = append(arc.Attachments, ArchiveFile{Name: rules.AttachmentDir + "/" + rel, Data: data})
		}
	}
	if !haveTranscript {
		return fail("the archive has no transcript")
	}
	return arc, nil
}

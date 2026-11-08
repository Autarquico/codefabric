package daemon

import (
	"archive/tar"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

const (
	// restoreMaxMetaBytes bounds the entries that are read into memory: the manifest and the sealed CA.
	restoreMaxMetaBytes = 1 << 20
	// restoreMaxBytes bounds what a restore writes, unless the caller says otherwise.
	restoreMaxBytes = 1 << 40
	dirMode         = 0o700
)

// RestoreOptions parameterise Restore.
type RestoreOptions struct {
	Paths config.Paths
	Clock clock.Clock // defaults to clock.Real
	// Backup is the file written by Backup.
	Backup string
	// Advertise is the host:port the restored Controller is reached at; its host
	// goes into the new server certificate. Empty keeps the one of the backup.
	Advertise string
	// Listen is the address the restored Controller binds. Empty keeps the one of the backup.
	Listen string
	// Passphrase opens the CA of the backup. Required when the backup holds one.
	Passphrase string
	// MaxBytes bounds what is written; 0 is 1 TiB.
	MaxBytes int64
}

// RestoreResult is what Restore tells the operator.
type RestoreResult struct {
	ControllerID  string `json:"controller_id"`
	SchemaVersion int64  `json:"schema_version"`
	Advertise     string `json:"advertise"`
	// CARestored is false when the backup had no CA: a new one was made, and every
	// client and Worker must join again.
	CARestored bool `json:"ca_restored"`
}

// Restore recreates the Controller of a backup in o.Paths, which must be empty
// or absent: it never merges into, or overwrites, a Controller. The result
// keeps the IDs and digests of the backup (the database and the blobs are the
// same bytes); only the address and the server certificate are new. The
// certificate is issued by the restored CA, so the Workers that pinned that CA
// only need the new address. It does not start the daemon.
//
// Nothing is written to the data or config directory until everything has been
// extracted, decrypted and verified in a staging directory next to each; the
// last step is two renames. A restore that fails leaves them as it found them.
func Restore(ctx context.Context, o RestoreOptions) (_ RestoreResult, err error) {
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = restoreMaxBytes
	}
	if o.Advertise != "" {
		if _, err := splitAdvertise(o.Advertise); err != nil {
			return RestoreResult{}, err
		}
	}
	p := o.Paths
	if within(p.ConfigDir, p.DataDir) || within(p.DataDir, p.ConfigDir) {
		return RestoreResult{}, domain.NewError(domain.CodeInvalidRequest, "the data and config directories must not contain each other", nil)
	}
	for _, dir := range []string{p.DataDir, p.ConfigDir} {
		if err := requireEmpty(dir); err != nil {
			return RestoreResult{}, err
		}
	}
	file, err := os.Open(o.Backup)
	if err != nil {
		return RestoreResult{}, domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("backup: %v", err), nil)
	}
	defer func() { _ = file.Close() }()

	stage, err := newStaging(p)
	if err != nil {
		return RestoreResult{}, err
	}
	defer stage.remove()

	x, err := extract(ctx, file, stage, o.MaxBytes)
	if err != nil {
		return RestoreResult{}, err
	}
	res, err := stage.finish(ctx, x, o)
	if err != nil {
		return RestoreResult{}, err
	}
	if err := stage.install(p); err != nil {
		return RestoreResult{}, err
	}
	return res, nil
}

// requireEmpty accepts a directory that is absent or has nothing in it.
func requireEmpty(dir string) error {
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("restore: %s cannot be used: %v", dir, err), nil)
	case len(entries) > 0:
		return domain.NewError(domain.CodeInvalidRequest, "restore only goes into an empty directory, and "+dir+" is not; nothing was written", nil)
	}
	return nil
}

func splitAdvertise(advertise string) (host string, err error) {
	host, port, splitErr := net.SplitHostPort(advertise)
	if n, convErr := strconv.Atoi(port); splitErr != nil || host == "" || convErr != nil || n < 1 || n > 65535 {
		return "", domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("--advertise must be host:port, got %q", advertise), nil)
	}
	return host, nil
}

// staging is where a restore is built before it is put in place.
type staging struct {
	data, config string // roots of the new data and config directories
}

func newStaging(p config.Paths) (*staging, error) {
	s := &staging{}
	for _, target := range []struct {
		dir  string
		root *string
	}{{p.DataDir, &s.data}, {p.ConfigDir, &s.config}} {
		parent := filepath.Dir(target.dir)
		if err := os.MkdirAll(parent, dirMode); err != nil {
			s.remove()
			return nil, fmt.Errorf("restore: %w", err)
		}
		root, err := os.MkdirTemp(parent, ".cf-restore-")
		if err != nil {
			s.remove()
			return nil, fmt.Errorf("restore: staging directory: %w", err)
		}
		*target.root = root
	}
	return s, nil
}

func (s *staging) remove() {
	for _, dir := range []string{s.data, s.config} {
		if dir != "" {
			_ = os.RemoveAll(dir)
		}
	}
}

// install puts the staged directories in place. They replace empty directories
// (rename does that atomically), so a failure of the second undoes the first.
func (s *staging) install(p config.Paths) error {
	if err := os.Rename(s.data, p.DataDir); err != nil {
		return fmt.Errorf("restore: installing %s: %w", p.DataDir, err)
	}
	if err := os.Rename(s.config, p.ConfigDir); err != nil {
		_ = os.Rename(p.DataDir, s.data)
		return fmt.Errorf("restore: installing %s: %w", p.ConfigDir, err)
	}
	return nil
}

// extracted is what extract found beyond the files it wrote.
type extracted struct {
	manifest BackupManifest
	sealedCA []byte
}

// extract writes the entries of the backup into the staging directories. It
// accepts exactly what Backup writes: the manifest first, then directories and
// regular files below data/ and config/, then the sealed CA. Anything else, an
// unsafe name or a second entry of the same name stops the restore.
func extract(ctx context.Context, r io.Reader, s *staging, maxBytes int64) (extracted, error) {
	var (
		x       extracted
		seen    = map[string]bool{}
		dirs    []dirTime
		written int64
	)
	tr := tar.NewReader(r)
	for first := true; ; first = false {
		if err := ctx.Err(); err != nil {
			return extracted{}, err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return extracted{}, badBackup("reading: %v", err)
		}
		name, err := cleanName(hdr.Name)
		if err != nil {
			return extracted{}, err
		}
		if seen[name] {
			return extracted{}, badBackup("%s appears twice", name)
		}
		seen[name] = true
		if first != (name == BackupManifestEntry) {
			return extracted{}, badBackup("the manifest must be the first entry, and the only one named so")
		}
		switch name {
		case BackupManifestEntry, BackupCAEntry:
			body, err := readSmall(tr, hdr)
			if err != nil {
				return extracted{}, err
			}
			if name == BackupCAEntry {
				x.sealedCA = body
			} else if err := json.Unmarshal(body, &x.manifest); err != nil || x.manifest.Format != BackupFormat {
				return extracted{}, badBackup("not a %s file", BackupFormat)
			}
			continue
		}
		root, rel, ok := s.locate(name)
		if !ok {
			return extracted{}, badBackup("unexpected entry %s", name)
		}
		dest := filepath.Join(root, filepath.FromSlash(rel))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, dirMode); err != nil {
				return extracted{}, err
			}
			dirs = append(dirs, dirTime{dest, hdr.ModTime})
		case tar.TypeReg:
			if written += hdr.Size; hdr.Size < 0 || written > maxBytes {
				return extracted{}, badBackup("it is larger than the %d bytes a restore accepts", maxBytes)
			}
			if err := writeFile(dest, tr, hdr); err != nil {
				return extracted{}, err
			}
		default:
			return extracted{}, badBackup("%s is a %s; only files and directories are restored", name, typeName(hdr.Typeflag))
		}
	}
	if x.manifest.Format == "" {
		return extracted{}, badBackup("it has no manifest")
	}
	if (x.manifest.CA == BackupCAEncrypted) != (x.sealedCA != nil) {
		return extracted{}, badBackup("the manifest and the entries disagree about the CA")
	}
	// Directories last and deepest first: writing a file changes its directory's mtime.
	slices.Reverse(dirs)
	for _, d := range dirs {
		_ = os.Chtimes(d.path, d.mtime, d.mtime)
	}
	return x, nil
}

type dirTime struct {
	path  string
	mtime time.Time
}

// locate maps an entry name to the staging root it belongs below and the path under it.
func (s *staging) locate(name string) (root, rel string, ok bool) {
	for prefix, root := range map[string]string{BackupDataPrefix: s.data, BackupConfigPrefix: s.config} {
		if rel, found := strings.CutPrefix(name, prefix); found && rel != "" {
			return root, rel, true
		}
	}
	return "", "", false
}

// cleanName returns the slash-separated name of an entry, refusing every
// absolute path and every one with a ".." or an empty element.
func cleanName(raw string) (string, error) {
	name := strings.TrimSuffix(raw, "/")
	if name == "" || strings.ContainsAny(name, "\x00\\") || path.IsAbs(name) || filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return "", badBackup("unsafe entry name %q", raw)
	}
	for _, el := range strings.Split(name, "/") {
		if el == "" || el == "." || el == ".." {
			return "", badBackup("unsafe entry name %q", raw)
		}
	}
	return name, nil
}

func readSmall(r io.Reader, hdr *tar.Header) ([]byte, error) {
	if hdr.Typeflag != tar.TypeReg || hdr.Size > restoreMaxMetaBytes {
		return nil, badBackup("%s is not a regular file of up to %d bytes", hdr.Name, restoreMaxMetaBytes)
	}
	return io.ReadAll(io.LimitReader(r, hdr.Size))
}

// writeFile creates dest, which must not exist, with the owner's bits of the
// header and its mtime (the garbage collector's grace period depends on it).
func writeFile(dest string, r io.Reader, hdr *tar.Header) error {
	if err := os.MkdirAll(filepath.Dir(dest), dirMode); err != nil {
		return err
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(hdr.Mode&0o700|0o600)) //nolint:gosec // dest is below our own staging directory; the name was vetted
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	if _, err := io.CopyN(f, r, hdr.Size); err != nil {
		_ = f.Close()
		return badBackup("%s is truncated: %v", hdr.Name, err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chtimes(dest, hdr.ModTime, hdr.ModTime)
}

func typeName(t byte) string {
	switch t {
	case tar.TypeSymlink:
		return "symlink"
	case tar.TypeLink:
		return "hardlink"
	case tar.TypeChar, tar.TypeBlock:
		return "device"
	case tar.TypeFifo:
		return "fifo"
	}
	return "special file (type " + string(rune(t)) + ")"
}

func badBackup(format string, args ...any) error {
	return domain.NewError(domain.CodeInvalidRequest, "restore: the backup is not valid: "+fmt.Sprintf(format, args...), nil)
}

// finish turns the extracted files into a Controller: it verifies the
// database, puts the CA in place (or makes one), reissues the server
// certificate and rewrites controller.json.
func (s *staging) finish(ctx context.Context, x extracted, o RestoreOptions) (RestoreResult, error) {
	db := filepath.Join(s.data, "controller.db")
	if err := integrityCheck(ctx, db); err != nil {
		return RestoreResult{}, fmt.Errorf("restore: the database of the backup failed verification: %w", err)
	}
	info, err := store.ReadBackupInfo(ctx, db)
	if err != nil {
		return RestoreResult{}, err
	}
	if info.ControllerID != x.manifest.ControllerID {
		return RestoreResult{}, badBackup("the manifest and the database name different Controllers")
	}

	ca, restored, err := s.certificateAuthority(o, x)
	if err != nil {
		return RestoreResult{}, err
	}

	staged := config.Paths{DataDir: s.data, ConfigDir: s.config}
	cfg, err := config.LoadController(staged)
	if err != nil {
		return RestoreResult{}, err
	}
	cfg.Advertise = cmpOr(o.Advertise, cfg.Advertise)
	cfg.Listen = cmpOr(o.Listen, cfg.Listen)
	host, err := splitAdvertise(cfg.Advertise)
	if err != nil {
		return RestoreResult{}, err
	}
	if _, err := ca.LoadServerCert(staged.PKIDir(), host, o.Clock.Now()); err != nil {
		return RestoreResult{}, err
	}
	if err := config.SaveController(staged, cfg); err != nil {
		return RestoreResult{}, err
	}
	return RestoreResult{ControllerID: info.ControllerID, SchemaVersion: info.SchemaVersion, Advertise: cfg.Advertise, CARestored: restored}, nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// certificateAuthority saves the CA of the backup in the staged pki directory,
// or a new one when the backup has none.
func (s *staging) certificateAuthority(o RestoreOptions, x extracted) (ca *pki.CA, restored bool, err error) {
	dir := config.Paths{DataDir: s.data}.PKIDir()
	if x.manifest.CA != BackupCAEncrypted {
		ca, err = pki.NewCA(o.Clock.Now())
		if err == nil {
			err = ca.Save(dir)
		}
		return ca, false, err
	}
	if o.Passphrase == "" {
		return nil, false, domain.NewError(domain.CodeInvalidRequest, "the backup holds the CA, encrypted: give --passphrase or --passphrase-file", nil)
	}
	payload, err := DecryptCA(bytes.NewReader(x.sealedCA), o.Passphrase)
	if err != nil {
		return nil, false, domain.NewError(domain.CodeInvalidRequest, "restore: the passphrase does not open the CA of the backup; nothing was written", nil)
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, false, err
	}
	for name, content := range map[string]string{"ca.pem": payload.CertPEM, "ca.key": payload.KeyPEM} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			return nil, false, err
		}
	}
	ca, err = pki.LoadCA(dir) // the pair must be a CA, and match
	return ca, true, err
}

// integrityCheck succeeds only when PRAGMA integrity_check reports exactly "ok"
// for the quiescent database at path.
func integrityCheck(ctx context.Context, path string) error {
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() + "?" + url.Values{"mode": {"ro"}, "immutable": {"1"}}.Encode()
	pool, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer func() { _ = pool.Close() }()
	rows, err := pool.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return err
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(lines) != 1 || lines[0] != "ok" {
		return fmt.Errorf("integrity_check: %s", strings.Join(lines, "; "))
	}
	return nil
}

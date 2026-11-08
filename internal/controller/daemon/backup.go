package daemon

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/support/flock"
)

// The backup is one tar file. What `cf restore` finds in it, and where:
//
//	manifest.json              first entry; BackupManifest
//	data/controller.db         the database, a VACUUM INTO copy; extracted to <data>/controller.db
//	data/blobs/...             the blob store, extracted below <data>
//	data/relay/...             the git relay (bare repositories, without hooks), below <data>
//	config/controller.json     the Controller's settings, extracted below <config>
//	pki/ca.age                 only with a passphrase: age(scrypt) of a CAPayload JSON
//
// Entries under data/ and config/ are paths relative to the data and config
// directories of the Controller; modes are limited to the owner's bits.
// Nothing else is ever in the file: no credentials, no secrets.json, nothing
// of <data>/worker. The entries are an allow-list, not a filter.
const (
	BackupFormat = "codefabric-backup/1"

	BackupManifestEntry = "manifest.json"
	BackupDataPrefix    = "data/"
	BackupConfigPrefix  = "config/"
	BackupCAEntry       = "pki/ca.age"

	BackupDatabaseEntry = BackupDataPrefix + "controller.db"

	// BackupCAEncrypted and BackupCAAbsent are the values of BackupManifest.CA.
	BackupCAEncrypted = "encrypted"
	BackupCAAbsent    = "absent"
)

// BackupManifest describes a backup; it is the first entry of the file.
type BackupManifest struct {
	Format        string    `json:"format"`
	CreatedAt     time.Time `json:"created_at"`
	ControllerID  string    `json:"controller_id"`
	SchemaVersion int64     `json:"schema_version"`
	CA            string    `json:"ca"` // BackupCAEncrypted or BackupCAAbsent
}

// CAPayload is what pki/ca.age holds once decrypted: the files of the CA as
// pki saves them (ca.pem, ca.key).
type CAPayload struct {
	CertPEM string `json:"ca_pem"`
	KeyPEM  string `json:"ca_key"`
}

// BackupOptions parameterise Backup.
type BackupOptions struct {
	Paths config.Paths
	Clock clock.Clock // defaults to clock.Real
	// Out is the file to create; it must not exist and must lie outside the data directory.
	Out string
	// Passphrase, when set, puts the CA in the backup, encrypted with it.
	Passphrase string
	// BetweenRelayPasses, for tests, runs in each repository of the relay after
	// its refs are copied and before the rest is. gc.lock is held when it runs.
	BetweenRelayPasses func(repo string)
}

// BackupResult is what Backup tells the operator.
type BackupResult struct {
	Path       string         `json:"path"`
	Bytes      int64          `json:"bytes"`
	CAIncluded bool           `json:"ca_included"`
	Manifest   BackupManifest `json:"manifest"`
}

// Backup writes a consistent backup of the Controller in o.Paths to o.Out. It
// runs on the Controller's host and is meant to run beside the live daemon:
// the database is a snapshot, and the garbage collector is held off (gc.lock)
// from before the snapshot until the relay is copied, so that nothing the
// snapshot names is unlinked meanwhile. Order: database, blobs, relay (the refs
// of each repository before its objects, so no copied ref names an object that
// is missing), config. The file appears at o.Out only when complete.
func Backup(ctx context.Context, o BackupOptions) (_ BackupResult, err error) {
	p := o.Paths
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	if _, err := os.Stat(p.DatabaseFile()); err != nil {
		return BackupResult{}, fmt.Errorf("%w: no Controller database at %s", config.ErrNotInitialized, p.DatabaseFile())
	}
	out, err := filepath.Abs(o.Out)
	if err != nil {
		return BackupResult{}, err
	}
	if within(out, p.DataDir) {
		return BackupResult{}, domain.NewError(domain.CodeInvalidRequest, "the backup file must be outside the data directory "+p.DataDir, nil)
	}
	if _, err := os.Lstat(out); err == nil {
		return BackupResult{}, domain.NewError(domain.CodeInvalidRequest, out+" already exists; backups never overwrite", nil)
	}
	ca, err := readCA(p, o.Passphrase)
	if err != nil {
		return BackupResult{}, err
	}

	scratch, err := os.MkdirTemp(filepath.Dir(out), ".cf-backup-")
	if err != nil {
		return BackupResult{}, fmt.Errorf("backup: scratch directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	partial := filepath.Join(scratch, "backup.tar")
	file, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // inside our own 0700 scratch directory
	if err != nil {
		return BackupResult{}, fmt.Errorf("backup: creating %s: %w", partial, err)
	}
	defer func() { _ = file.Close() }()

	release, err := flock.Lock(p.GCLockFile())
	if err != nil {
		return BackupResult{}, err
	}
	defer release()

	copyPath := filepath.Join(scratch, "controller.db")
	if err := store.VacuumInto(ctx, p.DatabaseFile(), copyPath); err != nil {
		return BackupResult{}, err
	}
	info, err := store.ReadBackupInfo(ctx, copyPath)
	if err != nil {
		return BackupResult{}, err
	}
	manifest := BackupManifest{
		Format: BackupFormat, CreatedAt: o.Clock.Now().UTC(), ControllerID: info.ControllerID,
		SchemaVersion: info.SchemaVersion, CA: BackupCAAbsent,
	}
	if ca != nil {
		manifest.CA = BackupCAEncrypted
	}

	w := &archive{tw: tar.NewWriter(file), seen: map[string]bool{}}
	steps := []func() error{
		func() error { return w.addJSON(BackupManifestEntry, manifest) },
		func() error { return w.addFile(BackupDatabaseEntry, copyPath) },
		func() error { return w.addTree(BackupDataPrefix+"blobs", filepath.Join(p.DataDir, "blobs"), nil) },
		func() error { return w.addRelay(p.RelayDir(), o.BetweenRelayPasses) },
		func() error { return w.addFile(BackupConfigPrefix+"controller.json", p.ControllerConfigFile()) },
	}
	for _, step := range steps {
		if err := ctx.Err(); err != nil {
			return BackupResult{}, err
		}
		if err := step(); err != nil {
			return BackupResult{}, err
		}
	}
	release() // the relay is copied; the garbage collector may run again
	if ca != nil {
		if err := w.addBytes(BackupCAEntry, ca); err != nil {
			return BackupResult{}, err
		}
	}
	if err := w.tw.Close(); err != nil {
		return BackupResult{}, fmt.Errorf("backup: closing the archive: %w", err)
	}
	if err := file.Sync(); err != nil {
		return BackupResult{}, fmt.Errorf("backup: syncing: %w", err)
	}
	size, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		return BackupResult{}, err
	}
	if err := file.Close(); err != nil {
		return BackupResult{}, err
	}
	if err := os.Rename(partial, out); err != nil {
		return BackupResult{}, fmt.Errorf("backup: installing %s: %w", out, err)
	}
	return BackupResult{Path: out, Bytes: size, CAIncluded: ca != nil, Manifest: manifest}, nil
}

// readCA returns the age-encrypted CA, or nil when no passphrase was given.
func readCA(p config.Paths, passphrase string) ([]byte, error) {
	if passphrase == "" {
		return nil, nil
	}
	if _, err := pki.LoadCA(p.PKIDir()); err != nil { // validates the pair and its permissions
		return nil, err
	}
	cert, err := os.ReadFile(filepath.Join(p.PKIDir(), "ca.pem")) //nolint:gosec // fixed name below our own pki directory
	if err != nil {
		return nil, err
	}
	key, err := os.ReadFile(filepath.Join(p.PKIDir(), "ca.key")) //nolint:gosec // idem
	if err != nil {
		return nil, err
	}
	plain, err := json.Marshal(CAPayload{CertPEM: string(cert), KeyPEM: string(key)})
	if err != nil {
		return nil, err
	}
	recipient, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return nil, fmt.Errorf("backup: passphrase: %w", err)
	}
	var sealed strings.Builder
	enc, err := age.Encrypt(&sealed, recipient)
	if err != nil {
		return nil, err
	}
	if _, err := enc.Write(plain); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return []byte(sealed.String()), nil
}

// DecryptCA is the inverse of what Backup does to the CA.
func DecryptCA(sealed io.Reader, passphrase string) (CAPayload, error) {
	identity, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return CAPayload{}, err
	}
	r, err := age.Decrypt(sealed, identity)
	if err != nil {
		return CAPayload{}, fmt.Errorf("backup: decrypting the CA (wrong passphrase?): %w", err)
	}
	var payload CAPayload
	if err := json.NewDecoder(r).Decode(&payload); err != nil {
		return CAPayload{}, fmt.Errorf("backup: reading the CA: %w", err)
	}
	return payload, nil
}

// archive writes the entries of the backup, each once.
type archive struct {
	tw   *tar.Writer
	seen map[string]bool
}

func (a *archive) addBytes(name string, data []byte) error {
	hdr := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg}
	if err := a.tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := a.tw.Write(data)
	a.seen[name] = true
	return err
}

func (a *archive) addJSON(name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return a.addBytes(name, data)
}

// addFile copies the regular file at path as name. A file that vanished
// meanwhile (the relay's scratch files) is skipped; one that is not a regular
// file is refused.
func (a *archive) addFile(name, path string) error {
	f, err := os.Open(path) //nolint:gosec // the caller names what the backup takes
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("backup: %s is not a regular file", path)
	}
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	scrub(hdr, name)
	if err := a.tw.WriteHeader(hdr); err != nil {
		return err
	}
	a.seen[name] = true
	if _, err := io.CopyN(a.tw, f, info.Size()); err != nil {
		return fmt.Errorf("backup: copying %s: %w", path, err)
	}
	return nil
}

func (a *archive) addDir(name string, info fs.FileInfo) error {
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	scrub(hdr, name+"/")
	a.seen[name] = true
	return a.tw.WriteHeader(hdr)
}

// scrub names the entry and drops who owns it and which bits others had.
func scrub(hdr *tar.Header, name string) {
	hdr.Name = name
	hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
	hdr.Mode &= 0o700
	hdr.AccessTime, hdr.ChangeTime = time.Time{}, time.Time{}
}

// addTree copies the directories and regular files below root as prefix/...,
// in lexical order, except those for which want says false (nil wants all).
// Symlinks and special files are not part of any tree the Controller makes
// and are left out. An entry already copied is not copied twice.
func (a *archive) addTree(prefix, root string, want func(rel string, d fs.DirEntry) bool) error {
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // vanished, or never created
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		name := prefix
		if rel != "." {
			name = prefix + "/" + filepath.ToSlash(rel)
		}
		if rel != "." && want != nil && !want(filepath.ToSlash(rel), d) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if a.seen[name] {
			return nil
		}
		switch {
		case d.IsDir():
			info, err := d.Info()
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			return a.addDir(name, info)
		case d.Type().IsRegular():
			return a.addFile(name, path)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("backup: copying %s: %w", root, err)
	}
	return nil
}

// addRelay copies the relay: for each bare repository, the refs first and then
// the rest (never the refs again), since objects are only ever added while the backup runs (the
// relay's own gc is off and the collector is held by gc.lock) and a ref is
// written after the objects it names. The relay's scratch directory is left out,
// and so are the hooks: the relay writes its own when it starts and git would
// run whatever a backup brought.
func (a *archive) addRelay(root string, betweenPasses func(repo string)) error {
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	prefix := BackupDataPrefix + "relay"
	if info, err := os.Stat(root); err == nil {
		if err := a.addDir(prefix, info); err != nil {
			return err
		}
	}
	for _, e := range entries {
		sub, path := prefix+"/"+e.Name(), filepath.Join(root, e.Name())
		switch {
		case e.Name() == "tmp" || e.Name() == "hooks":
			continue
		case e.IsDir() && strings.HasSuffix(e.Name(), ".git"):
			if err := a.addTree(sub, path, isRef); err != nil {
				return err
			}
			if betweenPasses != nil {
				betweenPasses(path)
			}
			if err := a.addTree(sub, path, restOfRepo); err != nil {
				return err
			}
		default:
			if err := a.addTree(sub, path, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

// isRef selects what names commits in a bare repository: HEAD, packed-refs and
// refs/ (whose directories are listed too, so the walk reaches the files).
func isRef(rel string, d fs.DirEntry) bool {
	if !allowedRepoEntry(strings.Split(rel, "/"), d.IsDir()) {
		return false
	}
	switch {
	case rel == "HEAD" || rel == "packed-refs":
		return !d.IsDir()
	case rel == "refs" || strings.HasPrefix(rel, "refs/"):
		return true
	}
	return false
}

// restOfRepo selects the rest of a bare repository: what Restore admits (see
// allowedRepoEntry), which leaves out the hooks, branches/, lock files and the
// like. The refs are
// not copied a second time: one created since the first pass could name objects
// that the walk of objects/ has already gone past.
func restOfRepo(rel string, d fs.DirEntry) bool {
	return !isRef(rel, d) && allowedRepoEntry(strings.Split(rel, "/"), d.IsDir())
}

// within says whether path is dir or below it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

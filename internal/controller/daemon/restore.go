package daemon

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Autarquico/codefabric/internal/app"
	"github.com/Autarquico/codefabric/internal/client"
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
	// Listen is the host:port the restored Controller binds. Empty keeps the one of the
	// backup, except that its port follows Advertise when that names another port.
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
	// Listen is where the daemon will bind.
	Listen string `json:"listen"`
	// OwnerID is the owner created for the operator of this host; its credentials
	// are in the config directory, as `cf controller init` leaves them.
	OwnerID string `json:"owner_id"`
	// CARestored is false when the backup had no CA: a new one was made, and every
	// client and Worker must join again.
	CARestored bool `json:"ca_restored"`
}

// Restore recreates the Controller of a backup in o.Paths, which must be empty
// or absent: it never merges into, or overwrites, a Controller. The result
// keeps the IDs and digests of the backup (the database and the blobs are the
// same bytes); only the address and the server certificate are new. The
// certificate is issued by the restored CA, so the Workers that pinned that CA
// only need the new address. A new owner is created, with its credentials in
// the config directory, so that the operator can administer the Controller of
// this host with or without the CA of the backup: the credentials are never in
// a backup. It does not start the daemon.
//
// Nothing is written to the data or config directory until everything has been
// extracted, decrypted and verified in a staging directory next to each; the
// last step is two renames. A restore that fails leaves them as it found them; a
// crash between the two renames leaves the data directory filled, and it must be
// emptied before trying again.
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
	if o.Listen != "" {
		if _, _, err := splitHostPort(o.Listen); err != nil {
			return RestoreResult{}, domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("--listen must be host:port, got %q", o.Listen), nil)
		}
	}
	p := o.Paths
	if err := RequireRestorable(p); err != nil {
		return RestoreResult{}, err
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

	x, err := extract(ctx, file, stage, o.MaxBytes, o.Passphrase != "")
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

// RequireRestorable fails unless a restore into p can start: the data and
// config directories are separate, and each is absent or empty.
func RequireRestorable(p config.Paths) error {
	if within(p.ConfigDir, p.DataDir) || within(p.DataDir, p.ConfigDir) {
		return domain.NewError(domain.CodeInvalidRequest, "the data and config directories must not contain each other", nil)
	}
	for _, dir := range []string{p.DataDir, p.ConfigDir} {
		if err := requireEmpty(dir); err != nil {
			return err
		}
	}
	return nil
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
	host, _, err = splitHostPort(advertise)
	if err != nil || host == "" {
		return "", domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("--advertise must be host:port, got %q", advertise), nil)
	}
	return host, nil
}

// splitHostPort is net.SplitHostPort with a port that is a number in 1..65535.
func splitHostPort(addr string) (host, port string, err error) {
	host, port, err = net.SplitHostPort(addr)
	if err != nil {
		return "", "", err
	}
	if n, convErr := strconv.Atoi(port); convErr != nil || n < 1 || n > 65535 {
		return "", "", fmt.Errorf("port %q is not in 1..65535", port)
	}
	return host, port, nil
}

// listenFor is the address the restored daemon binds. An explicit --listen wins.
// Otherwise the one of the backup stays, unless the advertised port is another
// one: then the daemon follows it (as `cf controller init` does), keeping the
// host of the old address only if it is a wildcard, since a concrete IP of the
// old host means nothing on this one.
func listenFor(explicit, advertise, backedUp string) string {
	if explicit != "" {
		return explicit
	}
	_, advPort, _ := splitHostPort(advertise)
	host, port, err := splitHostPort(backedUp)
	if err == nil && port == advPort {
		return backedUp
	}
	if err != nil || (host != "" && host != "0.0.0.0" && host != "::") {
		host = "0.0.0.0"
	}
	return net.JoinHostPort(host, advPort)
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

// install puts the staged directories in place. An absent target is renamed
// over; an empty one is removed first (rename cannot replace a directory on
// every platform). A failure of the second undoes the first, and a target that
// was there empty is there empty again.
func (s *staging) install(p config.Paths) error {
	_, statErr := os.Stat(p.DataDir)
	dataExisted := statErr == nil
	if err := replaceEmptyDir(s.data, p.DataDir); err != nil {
		return fmt.Errorf("restore: installing %s: %w", p.DataDir, err)
	}
	if err := replaceEmptyDir(s.config, p.ConfigDir); err != nil {
		_ = os.Rename(p.DataDir, s.data)
		if dataExisted {
			_ = os.Mkdir(p.DataDir, dirMode)
		}
		return fmt.Errorf("restore: installing %s: %w", p.ConfigDir, err)
	}
	return nil
}

// replaceEmptyDir renames from to target, which is absent or an empty
// directory (os.Remove refuses a directory that has anything in it). If the
// rename fails, an empty target that was there is put back.
func replaceEmptyDir(from, target string) error {
	removed := true
	if err := os.Remove(target); errors.Is(err, fs.ErrNotExist) {
		removed = false
	} else if err != nil {
		return err
	}
	if err := os.Rename(from, target); err != nil {
		if removed {
			_ = os.Mkdir(target, dirMode)
		}
		return err
	}
	return nil
}

// readManifest reads the first entry of the backup at path.
func readManifest(path string) (m BackupManifest, err error) {
	f, err := os.Open(path) //nolint:gosec // the backup the operator named
	if err != nil {
		return m, err
	}
	defer func() { _ = f.Close() }()
	tr := tar.NewReader(f)
	hdr, err := tr.Next()
	if err != nil || hdr.Name != BackupManifestEntry {
		return m, badBackup("it has no manifest")
	}
	body, err := readSmall(tr, hdr)
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(body, &m)
}

// extracted is what extract found beyond the files it wrote.
type extracted struct {
	manifest BackupManifest
	sealedCA []byte
}

// extract writes the entries of the backup into the staging directories. It
// accepts exactly what Backup writes (see allowedEntry): the manifest first,
// then directories and regular files at the listed places of data/ and
// config/, then the sealed CA. Anything else, an unsafe name or a second entry
// of the same name stops the restore, and so does an encrypted CA when
// havePassphrase is false, before the rest of the file is read.
func extract(ctx context.Context, r io.Reader, s *staging, maxBytes int64, havePassphrase bool) (extracted, error) {
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
			} else if x.manifest.CA == BackupCAEncrypted && !havePassphrase {
				return extracted{}, errNeedsPassphrase
			}
			continue
		}
		root, rel, ok := s.locate(name)
		if !ok || !allowedEntry(name, hdr.Typeflag == tar.TypeDir) {
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
			var body io.Reader = tr
			if isRepoConfig(name) {
				raw, err := readSmall(tr, hdr)
				if err != nil {
					return extracted{}, err
				}
				if err := checkRepoConfig(raw); err != nil {
					return extracted{}, badBackup("%s: %v", name, err)
				}
				body = bytes.NewReader(raw)
			}
			if err := writeFile(dest, body, hdr); err != nil {
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

var errNeedsPassphrase = domain.NewError(domain.CodeInvalidRequest,
	"the backup holds the CA, encrypted: give --passphrase or --passphrase-file; nothing was written", nil)

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

// repoTopLevel are the entries of a bare repository of the relay that a restore
// accepts. Not hooks/, which git runs, nor anything else that Backup does not
// write; config is accepted only if checkRepoConfig does.
var repoTopLevel = map[string]bool{
	"HEAD": false, "packed-refs": false, "config": false, "description": false, // files
	"objects": true, "refs": true, "logs": true, "info": true, // directories
}

// allowedEntry says whether Backup writes name, a cleaned entry name below data/
// or config/. This is the whole list:
//
//	data/controller.db                  config/controller.json
//	data/blobs/...                      data/relay (directory)
//	data/relay/<project>.git/{HEAD,packed-refs,config,description}
//	data/relay/<project>.git/{objects,refs,logs}/...
//	data/relay/<project>.git/info/exclude
//
// except objects/info/alternates and http-alternates, which would point the
// repository at objects elsewhere. The relay-level hooks/ and the hooks/ of a
// repository are not on it: the Controller writes the first itself when it
// starts, and git would run whatever else is there.
func allowedEntry(name string, isDir bool) bool {
	parts := strings.Split(name, "/")
	switch {
	case name == BackupDatabaseEntry || name == BackupConfigPrefix+"controller.json":
		return !isDir
	case parts[0] == "data" && len(parts) >= 2 && parts[1] == "blobs":
		return true
	case parts[0] == "data" && len(parts) >= 2 && parts[1] == "relay":
		if len(parts) == 2 {
			return isDir
		}
		if !strings.HasSuffix(parts[2], ".git") || len(parts[2]) == len(".git") {
			return false
		}
		return allowedRepoEntry(parts[3:], isDir)
	}
	return false
}

// allowedRepoEntry says whether the entry at parts, below a bare repository of
// the relay, travels in a backup. Backup and Restore both ask it, so that what
// one writes is never refused by the other. Lock files and FETCH_HEAD are
// transient and are not on the list.
func allowedRepoEntry(parts []string, isDir bool) bool {
	if len(parts) == 0 {
		return isDir
	}
	if !isDir && strings.HasSuffix(parts[len(parts)-1], ".lock") {
		return false
	}
	dirOnly, known := repoTopLevel[parts[0]]
	switch {
	case !known:
		return false
	case len(parts) == 1:
		return isDir == dirOnly
	case !dirOnly:
		return false
	case parts[0] == "info":
		return len(parts) == 2 && parts[1] == "exclude" && !isDir
	case parts[0] == "objects" && len(parts) >= 3 && parts[1] == "info":
		return parts[2] != "alternates" && parts[2] != "http-alternates"
	}
	return true
}

// isRepoConfig says whether name is the config file of a bare repository of the relay.
func isRepoConfig(name string) bool {
	parts := strings.Split(name, "/")
	return len(parts) == 4 && parts[1] == "relay" && parts[3] == "config"
}

// safeRepoConfig lists, lowercased, the keys a repository of the relay may
// have, and which values each takes. Everything else in a git config can run a
// program (core.hooksPath, core.fsmonitor, core.sshCommand, filter.*, include...).
var safeRepoConfig = map[string]func(string) bool{
	"core.repositoryformatversion": anyValue, "core.filemode": anyValue, "core.bare": anyValue,
	"core.logallrefupdates": anyValue, "core.ignorecase": anyValue, "core.precomposeunicode": anyValue,
	"core.fsmonitor":              func(v string) bool { return v == "false" },
	"receive.denynonfastforwards": anyValue, "receive.denydeletes": anyValue, "receive.fsckobjects": anyValue,
	"receive.advertisepushoptions": anyValue, "gc.auto": anyValue, "uploadpack.allowfilter": anyValue,
}

var (
	configSection = regexp.MustCompile(`^\[([A-Za-z]+)\]$`)
	configPair    = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9]*)\s*=\s*([A-Za-z0-9._-]+)$`)
)

func anyValue(string) bool { return true }

// checkRepoConfig accepts a git config made only of sections without
// subsections and of the keys of safeRepoConfig, with plain values.
func checkRepoConfig(raw []byte) error {
	section := ""
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if m := configSection.FindStringSubmatch(line); m != nil {
			section = strings.ToLower(m[1])
			continue
		}
		m := configPair.FindStringSubmatch(line)
		if m == nil || section == "" {
			return fmt.Errorf("the line %q is not one a repository of the relay has", line)
		}
		if check, ok := safeRepoConfig[section+"."+strings.ToLower(m[1])]; !ok || !check(m[2]) {
			return fmt.Errorf("the setting %s.%s = %s is not one a repository of the relay has", section, m[1], m[2])
		}
	}
	return nil
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
// certificate, creates the owner of this host, moves the counters past
// anything a Worker may have seen and rewrites controller.json.
func (s *staging) finish(ctx context.Context, x extracted, o RestoreOptions) (RestoreResult, error) {
	db := filepath.Join(s.data, "controller.db")
	if err := store.CheckIntegrity(ctx, db); err != nil {
		return RestoreResult{}, fmt.Errorf("restore: the database of the backup failed verification: %w", err)
	}
	info, err := store.ReadBackupInfo(ctx, db)
	if err != nil {
		return RestoreResult{}, err
	}
	if info.ControllerID != x.manifest.ControllerID {
		return RestoreResult{}, badBackup("the manifest and the database name different Controllers")
	}
	latest, err := store.LatestSchemaVersion()
	if err != nil {
		return RestoreResult{}, err
	}
	if info.SchemaVersion > latest {
		return RestoreResult{}, domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf(
			"restore: the backup has schema %d and this cf only knows up to %d: restore it with a cf at least as new as the one that made it", info.SchemaVersion, latest), nil)
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
	cfg.Advertise = cmp.Or(o.Advertise, cfg.Advertise)
	host, err := splitAdvertise(cfg.Advertise)
	if err != nil {
		return RestoreResult{}, err
	}
	cfg.Listen = listenFor(o.Listen, cfg.Advertise, cfg.Listen)
	if _, err := ca.LoadServerCert(staged.PKIDir(), host, o.Clock.Now()); err != nil {
		return RestoreResult{}, err
	}
	owner, err := s.createOwner(ctx, staged, ca, cfg.Advertise, o.Clock)
	if err != nil {
		return RestoreResult{}, err
	}
	if err := config.SaveController(staged, cfg); err != nil {
		return RestoreResult{}, err
	}
	if !restored {
		if err := s.releaseGoneWorkers(ctx, staged, o.Clock); err != nil {
			return RestoreResult{}, err
		}
	}
	// Last, and with the database closed: it leaves no side files.
	if err := store.AdvancePastRestore(ctx, db, o.Clock.Now()); err != nil {
		return RestoreResult{}, err
	}
	return RestoreResult{
		ControllerID: info.ControllerID, SchemaVersion: info.SchemaVersion, Advertise: cfg.Advertise, Listen: cfg.Listen,
		OwnerID: string(owner), CARestored: restored,
	}, nil
}

// releaseReasonRestoredWithoutCA is why the reservations of the Workers of a
// backup without its CA are closed: no identity of theirs is valid any more.
const releaseReasonRestoredWithoutCA = "restored_without_ca"

// releaseGoneWorkers ends what the Workers of the backup still held when no
// identity of theirs can come back: their live Runs are lost, as the
// reconciliation loses them, and their reservations are released, so that
// cf worker remove accepts them and nothing waits for them.
func (s *staging) releaseGoneWorkers(ctx context.Context, staged config.Paths, clk clock.Clock) (err error) {
	db, err := store.Open(ctx, staged.DatabaseFile(), store.Options{Clock: clk})
	if err != nil {
		return fmt.Errorf("restore: opening the restored database: %w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	services := app.New(db, clk, domain.NewIDGenerator(clk.Now, rand.Reader), app.Config{})
	if _, err := services.Reconciliation.ReleaseReservationsOfGoneWorkers(ctx, releaseReasonRestoredWithoutCA); err != nil {
		return fmt.Errorf("restore: releasing what the Workers held: %w", err)
	}
	return nil
}

// createOwner does what `cf controller init` does for the first owner: an
// identity signed by ca, with its credentials in the config directory. The
// owner can create the join tokens of everything else, which is what a Controller
// whose CA is new needs: no identity of the old one is valid there.
func (s *staging) createOwner(ctx context.Context, staged config.Paths, ca *pki.CA, advertise string, clk clock.Clock) (_ domain.ID, err error) {
	host, port, err := net.SplitHostPort(advertise)
	if err != nil {
		return "", err
	}
	db, err := store.Open(ctx, staged.DatabaseFile(), store.Options{Clock: clk})
	if err != nil {
		return "", fmt.Errorf("restore: opening the restored database: %w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	key, csr, err := client.GenerateKeyAndCSR()
	if err != nil {
		return "", err
	}
	keyPEM, err := client.EncodeKey(key)
	if err != nil {
		return "", err
	}
	hostname, _ := os.Hostname()
	owner, err := newAuthority(db, ca, clk, host, port).BootstrapOwner(ctx, csr, config.MachineName(hostname, "owner"))
	if err != nil {
		return "", err
	}
	err = client.SaveCredentials(staged.CredentialsDir(), client.Credentials{
		Controller: advertise, CertPEM: owner.Certificate, KeyPEM: keyPEM, CAPEM: ca.CertPEM(),
	})
	return owner.ID, err
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
		return nil, false, errNeedsPassphrase
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

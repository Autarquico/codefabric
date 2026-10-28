// Package secretjit hands the secret values of a Run to its run directory just
// in time, lets the wrapper and the redactors read them, and releases them.
//
// A delivery lives in <run dir>/secrets/ (0700, owned by the Worker's user):
// manifest.json (0600) lists each ref with the file that holds its value and
// whether the harness may read it; each value is in its own 0600 file, named
// by the first 32 hex digits of the SHA-256 of its ref. The manifest never
// holds a value.
//
// Two kinds of reader see a delivery, and they are kept apart:
//
//   - access: Resolver is what the wrapper uses to put a secret in the
//     harness's environment. It returns only refs the harness may read, and
//     Revoke takes that away for good;
//   - retention: Load is what the redaction of the output and the publication
//     guard use. It returns every value, revoked or not, until Release.
//
// The executor releases a delivery only once the capture of the output and
// the publication of the Run are closed, because until then the values are
// needed to sanitise them.
//
// Every change is a protocol that a crash at any point leaves recoverable, run
// under an exclusive lock on the run directory and fenced by the caller:
//
//   - Deliver builds the delivery in secrets.staging/, syncs every file and the
//     directory, asks the caller to verify that the Run may still receive it,
//     and only then renames it into place, retiring any older delivery to
//     secrets.deleting/ first. secrets/ is therefore either absent or a
//     complete delivery; it is never half written. A delivery records the
//     attempt that made it, and an older attempt cannot replace a newer one.
//
//   - Release removes secrets.staging/ and secrets.deleting/, and then, if the
//     caller verifies that the Run may let its secrets go, renames secrets/ to
//     secrets.deleting/ and removes it. It reads nothing, so a missing or
//     corrupt manifest does not stop it. It succeeds only when none of the
//     three directories is left; a failure to remove is ErrPending, to retry,
//     and never success.
//
//   - Sweep is the janitor: it runs Release over every run directory, letting
//     the caller decide which Runs are done.
//
//   - Render registers the files of the Run that will carry secret values (its
//     rendered configuration), synced, before they are written, all under the
//     same lock and fence; Release removes them with the delivery and leaves the
//     registry empty, and the janitor finishes what a crash left.
//
// Release is a logical deletion: the files are unlinked and the unlinks
// synced. It does not promise that the bytes are gone from the disk, from an
// SSD's spare blocks, from snapshots or from backups.
//
// No error of this package carries a value: errors name refs and paths only.
package secretjit

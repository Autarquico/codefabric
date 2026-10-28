// Package authsync keeps the auth files of the Runs of a Worker in step with
// the auth of the host.
//
// A harness that rewrites its auth in place with no lock (Codex truncates and
// writes it when it refreshes a token) would corrupt a file shared by two
// Runs, so each Run has a private copy (harness.ApplyResult.AuthCopies). A
// refresh also rotates the refresh token: a copy that stays behind loses its
// session. The [Broker] polls the auth of the host and the copies of the Runs
// that are registered, and propagates the most recent valid version to the
// others.
//
// The rules are few, and each protects a file from a harm:
//
//   - Only a version the adapter accepts (harness.AuthSyncer) is propagated; a
//     file the harness is halfway through writing is not one, and is tried
//     again at the next poll.
//   - Only a version with a stamp strictly later than another replaces that
//     one; a tie, or a version with no stamp, replaces nothing.
//   - A file that is missing or invalid is never replaced: it is not the
//     Broker's to create, and it may be in the middle of a write.
//   - Every replacement is a temporary file renamed over the target, 0600, so
//     a reader sees the old file or the new one.
//   - The content of a file is never logged.
package authsync

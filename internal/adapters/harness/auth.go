package harness

import "time"

// AuthSyncer is implemented by an adapter whose harness rewrites its auth
// file in place, with no lock (it truncates and writes), when it refreshes a
// token. Two Runs sharing one file through a link could corrupt it, so
// ApplyConfig gives each Run a copy ([ApplyResult].AuthCopies) and the Worker
// keeps the copies and the auth of the host in step: the most recent valid
// version is propagated to the rest, always by an atomic replacement.
//
// The Worker knows nothing of the format of the file; the adapter does.
type AuthSyncer interface {
	// GlobalAuth is the path of the auth file of the host, the one the copies
	// are taken from.
	GlobalAuth(host Host) string
	// AuthStamp says how recent a version of the auth file is, and whose
	// account it is. A content that is not an auth the harness would accept
	// (a file half written, for one) is an error and is never propagated. The
	// zero time is a valid auth that says nothing of its age, and is never
	// taken as the most recent. The identity is the account the auth belongs
	// to, opaque to the Worker; empty says the auth has none (an API key), and
	// such an auth is never propagated nor replaced: only versions of one
	// account are brought in step, so a Run cannot put another account in the
	// host or in the other Runs.
	AuthStamp(content []byte) (stamp time.Time, identity string, err error)
}

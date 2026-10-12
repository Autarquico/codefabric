package gitctx

import "github.com/Autarquico/codefabric/internal/support/redact"

// ResolveRefWithin is ResolveRef with the deadline of ls-remote given.
var ResolveRefWithin = resolveRef

// Redact exposes the shared redaction to the tests.
var Redact = redact.Text

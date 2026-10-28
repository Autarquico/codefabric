// Package harness is the port of the programs that do the work of a Run.
//
// [Adapter] is the interface of §7.2 of docs/codefabric-spec.md, adapted to the
// Runtime + wrapper model of the executor (Appendix B5 §6.4): the process of a
// Run belongs to tmux and to `cf worker exec`, never to the adapter. That
// model is where it departs from §7.2:
//
//   - Every method that touches the machine receives the [Host] it runs on;
//     Start, Resume and Stop included.
//   - Start and Resume do not start anything: they build the [Launch] (argv,
//     env and the files it needs) that the wrapper execs. They are idempotent,
//     because the executor prepares a Run again when it recovers it.
//   - The handle is a [Handle]: a value the Worker rebuilds from disk, with no
//     pid in it. Stop is an optional, courteous stop; the authoritative one is
//     the signal to the process group, which only the wrapper and the daemon
//     send.
//   - Events parses a source the caller opens and closes (the followed
//     output.log or a recorded file): Events(ctx, Handle, io.ReadCloser).
//   - Install takes [InstallOptions] and returns the [InstallPlan] it ran, or
//     would run under DryRun.
//   - ApplyConfig returns an [ApplyResult], which names the auth links it
//     made, the only symlinks a configuration digest leaves out.
//   - CollectUsage, Checkpoint and SendInput receive the [Handle] too.
//   - There is one interface. An adapter embeds [Unsupported] and implements
//     what it supports; [Capabilities] declares it, and [Capabilities.Modes]
//     is the only source of the modes it supports.
//
// What an adapter cannot do it reports with an error for which
// errors.Is(err, [ErrUnsupported]) holds, and a zero value: never data made up
// to look real.
//
// # Ownership of configuration
//
// A [Component] is managed (CodeFabric owns it) or local (the person owns
// it). ApplyConfig honours that by target:
//
//   - [TargetRun] writes into an empty directory: the configuration of one
//     Run, materialised from the snapshot alone (§8.12).
//   - [TargetGlobal] writes into the configuration of the Worker: it keeps
//     every local component, deletes only managed ones, and in a json-subset
//     component it replaces only the fields the snapshot carries and keeps
//     the rest of the file.
//
// # Secrets
//
// A secret never travels as a value. A [Launch] names it as a [SecretEnv],
// a reference the wrapper resolves just before exec; a [File] marks it in its
// content as {{cf-secret:<ref>}} (see [SecretMarker]) and lists the refs in
// File.SecretRefs. Digests are taken over the unrendered content, so they
// change with the ref and never with the value.
//
// The values of the markers of a Run's configuration are written by the
// Worker into the configuration of that Run only (a TargetRun apply leaves
// the markers as they are), after the Controller has granted them, with the
// [Escape] that the adapter names for each file ([SecretEscaper]).
package harness

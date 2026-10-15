// Package fakeharness is a harness for tests: a program that does what a JSON
// script tells it, and the [Adapter] that drives it through the whole
// harness port.
//
// # The program
//
// [Main] is the program. A test binary becomes it when its TestMain calls
// [RunIfRequested] and it is started with CF_TEST_AS_FAKEHARNESS=1; the
// binary in cmd/fakeharness is the same program on its own.
//
// It reads its [Script] from the first of these that is given, and never
// falls through to the next when the one given cannot be read:
//
//  1. the --script flag;
//  2. the FAKEHARNESS_SCRIPT variable;
//  3. fakeharness.json in the directory of FAKEHARNESS_CONFIG_DIR.
//
// With none of them it exits with [ExitScriptError]. It runs the steps in
// order and exits 0 at the end, unless a step says otherwise:
//
//   - emit writes one [Line] of JSONL to stdout: the events the Adapter
//     parses.
//   - record writes a [Record] of the argv, the filtered environment, the
//     working directory and the treedigest of the configuration directory.
//   - wait_file waits for a file to exist, for at most its timeout.
//   - read_line reads one line of stdin, for at most its timeout.
//   - ignore_signals makes SIGTERM, SIGINT and SIGHUP do nothing.
//   - exit ends with its code.
//
// A step that times out exits with [ExitTimeout]; one that fails, and a
// script or a flag that is wrong, with [ExitScriptError].
//
// # The adapter
//
// [Adapter] keeps its native configuration in ~/.fakeharness of the Host:
// settings.json (a json-subset component), fakeharness.json (the script, a
// file component), skills/<name> (one dir component each) and
// credentials.json, the auth, which no snapshot carries and a Run
// configuration links to.
package fakeharness

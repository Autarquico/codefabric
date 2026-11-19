#!/bin/sh
# Code Flock installer: curl -fsSL https://github.com/Autarquico/codeflock/releases/latest/download/install.sh | sh
#
# Env:  CFLK_VERSION=vX.Y.Z|latest  release to install (default latest, the newest non-prerelease)
#       CFLK_INSTALL_DIR            where cflk goes (default ~/.local/bin)
#       CFLK_RELEASE_BASE_URL       testing/mirror override of the download base; never skips verification
#       CF_VERSION, CF_INSTALL_DIR, CF_RELEASE_BASE_URL are still read, with a warning; CFLK_* wins when both are set
# Exit: 2 invalid arguments, 3 unsupported platform, 4 prerequisite failure, 5 download/checksum/signature failure
#
# It verifies the signed checksum list, then the tarball, and only then installs cflk and validates it.
# Only after that does it look at an existing cf: the cf of a Code Flock beta (or an older transition cf) is replaced
# by the transition cf (which runs cflk); any other cf is left alone. A cf elsewhere on the PATH is only reported, never run.
# The trust root is the allowed_signers block below. While it holds the PLACEHOLDER no signature verifies,
# so the installer fails closed. Review-first users verify this file with install.sh.sig.
set -eu
# a CDPATH would make `cd` print the directory it picks and can pick another one
unset CDPATH

DEFAULT_BASE_URL='https://github.com/Autarquico/codeflock/releases/download'
LATEST_URL='https://github.com/Autarquico/codeflock/releases/latest'
NAMESPACE='codefabric-release'
PRINCIPAL='release@codefabric'

TRANSITION_ANSWER='code-flock-cf-transition'
PROBE_SECONDS=5

log() { printf 'install.sh: %s\n' "$*" >&2; }
die() { code=$1; shift; log "error: $*"; exit "$code"; }

write_trust_roots() {
	cat >"$WORK/allowed_signers" <<'TRUST'
# BEGIN allowed_signers
release@codefabric namespaces="codefabric-release" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOZwPPPOq0JVZPk3SPcMUDHnCNq5WLxZwBXDdpGq8s1p
# END allowed_signers
TRUST
	cat >"$WORK/revoked_keys" <<'TRUST'
# BEGIN revoked_keys
# END revoked_keys
TRUST
	cat >"$WORK/selftest_signers" <<'TRUST'
selftest@codefabric namespaces="codefabric-release" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAICK4UoHMqGP3DPuaMooT7NdUdvyWM6Q+8IbsdGos1k20
TRUST
	cat >"$WORK/selftest.sig" <<'TRUST'
-----BEGIN SSH SIGNATURE-----
U1NIU0lHAAAAAQAAADMAAAALc3NoLWVkMjU1MTkAAAAgIrhSgcyoY/cM+5oyihPs11R2/J
YzpD7whux0aizWTbQAAAASY29kZWZhYnJpYy1yZWxlYXNlAAAAAAAAAAZzaGE1MTIAAABT
AAAAC3NzaC1lZDI1NTE5AAAAQDQNRDopCkBtnMEGvpI533Tck0mZ39LawrOk/yWhw/rlpG
otEVuooXgRC/DgIuRfHNZuWvrRniZWSWmgnVXfPAM=
-----END SSH SIGNATURE-----
TRUST
	printf 'codefabric-installer-selftest\n' >"$WORK/selftest.msg"
}

have() { command -v "$1" >/dev/null 2>&1; }

detect_platform() {
	case $(uname -s) in
		Linux) OS=linux ;;
		Darwin) OS=darwin ;;
		*) die 3 "unsupported OS: $(uname -s)" ;;
	esac
	case $(uname -m) in
		x86_64 | amd64) ARCH=amd64 ;;
		aarch64 | arm64) ARCH=arm64 ;;
		*) die 3 "unsupported architecture: $(uname -m)" ;;
	esac
	# A shell running under Rosetta on Apple silicon reports x86_64; the native binary is the right one.
	if [ "$OS" = darwin ] && [ "$ARCH" = amd64 ] && have sysctl && [ "$(sysctl -n sysctl.proc_translated 2>/dev/null)" = 1 ]; then
		ARCH=arm64
	fi
}

require_commands() {
	for c in curl tar awk sed grep mktemp ssh-keygen cp chmod mv mkdir rm cat wc tr basename dirname readlink cmp id ls; do
		have "$c" || die 4 "required command not found: $c"
	done
	have sha256sum || have shasum || die 4 'required command not found: sha256sum or shasum'
}

verify_signature() { # file signature
	ssh-keygen -Y verify -f "$WORK/allowed_signers" -r "$WORK/revoked_keys" \
		-I "$PRINCIPAL" -n "$NAMESPACE" -s "$2" <"$1" >/dev/null 2>&1
}

verify_the_verifier() {
	ssh-keygen -Y verify -f "$WORK/selftest_signers" -I selftest@codefabric -n "$NAMESPACE" \
		-s "$WORK/selftest.sig" <"$WORK/selftest.msg" >/dev/null 2>&1 ||
		die 4 'ssh-keygen cannot verify OpenSSH signatures (OpenSSH 8.2 or newer is required)'
}

# pick NEW_NAME NEW_VALUE OLD_NAME OLD_VALUE: the CFLK_* value, else the CF_* one (warning); sets PICKED and PICKED_NAME.
pick() {
	PICKED=$2
	PICKED_NAME=$1
	if [ -n "$4" ]; then
		if [ -n "$2" ]; then
			log "warning: $3 is ignored because $1 is set"
		else
			PICKED=$4
			PICKED_NAME=$3
			log "warning: $3 is deprecated and goes away in 0.5; use $1"
		fi
	fi
}

read_env() {
	pick CFLK_VERSION "${CFLK_VERSION:-}" CF_VERSION "${CF_VERSION:-}"
	REQUESTED=${PICKED:-latest}
	REQUESTED_NAME=$PICKED_NAME
	pick CFLK_INSTALL_DIR "${CFLK_INSTALL_DIR:-}" CF_INSTALL_DIR "${CF_INSTALL_DIR:-}"
	INSTALL_DIR=${PICKED:-$HOME/.local/bin}
	pick CFLK_RELEASE_BASE_URL "${CFLK_RELEASE_BASE_URL:-}" CF_RELEASE_BASE_URL "${CF_RELEASE_BASE_URL:-}"
	RELEASE_BASE_OVERRIDE=$PICKED
}

fetch() { # url destination
	if [ -n "$RELEASE_BASE_OVERRIDE" ]; then
		curl -fsSL --retry 2 -o "$2" "$1" || die 5 "download failed: $1"
	else
		curl -fsSL --proto '=https' --tlsv1.2 --retry 2 -o "$2" "$1" || die 5 "download failed: $1"
	fi
}

is_version() {
	[ "$(printf '%s\n' "$1" | wc -l | tr -d ' ')" -eq 1 ] || return 1
	printf '%s\n' "$1" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'
}

sha256_of() {
	if have sha256sum; then sha256sum "$1" | awk '{print $1}'; else shasum -a 256 "$1" | awk '{print $1}'; fi
}

# The newest non-prerelease, from where the latest-release URL redirects to (.../tag/vX.Y.Z).
latest_version() {
	if [ -n "$RELEASE_BASE_OVERRIDE" ]; then
		url="${RELEASE_BASE_OVERRIDE%/}/latest"
		final=$(curl -fsSL --retry 2 -o /dev/null -w '%{url_effective}' "$url") || die 5 "download failed: $url"
	else
		url=$LATEST_URL
		final=$(curl -fsSL --proto '=https' --tlsv1.2 --retry 2 -o /dev/null -w '%{url_effective}' "$url") || die 5 "download failed: $url"
	fi
	VERSION=${final##*/}
	is_version "$VERSION" || die 5 "could not resolve the latest release from $final"
}

resolve_version() {
	if [ "$REQUESTED" = latest ]; then
		latest_version
	else
		is_version "$REQUESTED" || die 2 "invalid $REQUESTED_NAME: $REQUESTED (vX.Y.Z or latest)"
		VERSION=$REQUESTED
	fi
	log "installing $VERSION"
}

# The signed list: line 1 "version: vX.Y.Z", then "<sha256>  cflk_<ver>_<os>_<arch>.tar.gz" lines only.
# A release from before the rename lists cf_<ver>_... instead: its tarballs hold no cflk, and the beta installer
# that shipped with it is the one that installs it.
check_checksums() {
	header=$(sed -n 1p "$WORK/checksums.txt")
	[ "$header" = "version: $VERSION" ] || die 5 "checksums.txt is signed for '${header#version: }', not $VERSION"
	if sed 1d "$WORK/checksums.txt" | grep -Eq '^[0-9a-f]{64}  cf_'; then
		old_dir=
		[ "$INSTALL_DIR" = "$HOME/.local/bin" ] || old_dir=" CF_INSTALL_DIR='$INSTALL_DIR'"
		die 5 "$VERSION predates the cflk rename (its assets are cf_*, not cflk_*); install it with the installer published with that release: curl -fsSL $BASE_URL/$VERSION/install.sh | CF_VERSION=$VERSION$old_dir sh"
	fi
	if sed 1d "$WORK/checksums.txt" | grep -Evq '^[0-9a-f]{64}  cflk_[0-9A-Za-z.+-]+_(linux|darwin)_(amd64|arm64)\.tar\.gz$'; then
		die 5 'checksums.txt contains a malformed line'
	fi
}

# Only the regular files `cflk` and `cf` leave the tarball, and they land in WORK, never in the install dir.
extract_binaries() {
	tar -tzf "$1" >"$WORK/members" || die 5 "$(basename "$1") is not readable"
	if grep -Eq '(^/|(^|/)\.\.(/|$))' "$WORK/members"; then die 5 "$(basename "$1") has an unsafe member path"; fi
	mkdir "$WORK/unpacked"
	for bin in cflk cf; do
		[ "$(grep -cx "$bin" "$WORK/members")" -eq 1 ] || die 5 "$(basename "$1") does not contain exactly one $bin"
		tar -xzf "$1" -C "$WORK/unpacked" "$bin" || die 5 "could not extract $bin from $(basename "$1")"
		if [ ! -f "$WORK/unpacked/$bin" ] || [ -L "$WORK/unpacked/$bin" ]; then die 5 "$bin in $(basename "$1") is not a regular file"; fi
	done
}

download_tarball() {
	name="cflk_${VERSION#v}_${OS}_${ARCH}.tar.gz"
	fetch "$BASE_URL/$VERSION/checksums.txt" "$WORK/checksums.txt"
	fetch "$BASE_URL/$VERSION/checksums.txt.sig" "$WORK/checksums.txt.sig"
	verify_signature "$WORK/checksums.txt" "$WORK/checksums.txt.sig" || die 5 'signature verification failed: checksums.txt'
	check_checksums
	expected=$(awk -v n="$name" 'NR > 1 && $2 == n { print $1; c++ } END { exit c != 1 }' "$WORK/checksums.txt") ||
		die 5 "$name is not listed exactly once in the signed checksums"
	fetch "$BASE_URL/$VERSION/$name" "$WORK/$name"
	[ "$(sha256_of "$WORK/$name")" = "$expected" ] || die 5 "checksum mismatch for $name"
	extract_binaries "$WORK/$name"
}

# place SRC DEST: copy next to DEST, then rename over it, so a reader never sees half a file.
# Fails (leaving no TMP_BIN behind that the exit trap does not remove) when DEST's directory is not writable.
place() {
	dest_dir=$(dirname "$2")
	TMP_BIN=$(mktemp "$dest_dir/.$(basename "$2").XXXXXX") || return 1
	{ cp "$1" "$TMP_BIN" && chmod 755 "$TMP_BIN"; } || return 1
}

install_cflk() {
	place "$WORK/unpacked/cflk" "$DIR/cflk" || die 4 "cannot write to $DIR"
	"$TMP_BIN" version >"$WORK/cflk.version" 2>&1 </dev/null || die 4 'the new cflk does not run; nothing was installed'
	# the version is a whole word of the output: 0.4.0 is not 0.4.0-beta.1, nor 10.4.0
	awk -v v="${VERSION#v}" '{ for (i = 1; i <= NF; i++) if ($i == v) found = 1 } END { exit !found }' "$WORK/cflk.version" ||
		die 4 "the new cflk does not report $VERSION; nothing was installed"
	mv -f "$TMP_BIN" "$DIR/cflk" || die 4 "cannot install into $DIR"
	TMP_BIN=
	log "installed $DIR/cflk ($VERSION)"
}

# resolve_link PATH: PATH with every symlink followed, made absolute; fails on a loop or a dangling link.
resolve_link() {
	p=$1
	hops=0
	while [ -L "$p" ]; do
		hops=$((hops + 1))
		[ "$hops" -le 20 ] || return 1
		target=$(readlink "$p") || return 1
		case $target in
			/*) p=$target ;;
			*) p=$(dirname "$p")/$target ;;
		esac
	done
	[ -e "$p" ] || return 1
	parent=$(cd -P "$(dirname "$p")" && pwd -P) || return 1
	printf '%s/%s\n' "$parent" "$(basename "$p")"
}

# run_limited SECONDS CMD...: CMD with its own stdio, killed when it does not finish in time (so a foreign cf that
# waits for a terminal cannot hang the installer). Uses timeout(1) when there is one; macOS has none.
run_limited() {
	secs=$1
	shift
	if have timeout; then
		timeout -s KILL "$secs" "$@"
		return
	fi
	"$@" &
	cmd_pid=$!
	(
		trap 'kill "$sleep_pid"; exit 0' TERM
		sleep "$secs" &
		sleep_pid=$!
		wait "$sleep_pid"
		kill -9 "$cmd_pid"
	) >/dev/null 2>&1 &
	killer_pid=$!
	status=0
	wait "$cmd_pid" || status=$?
	kill "$killer_pid" >/dev/null 2>&1 || true
	wait "$killer_pid" >/dev/null 2>&1 || true
	return "$status"
}

# The version of a Code Flock beta from what its `version --json` printed: exactly {"version","commit","date"}, and
# the version is 0.4.0-beta.N or older. Prints it, or fails.
beta_version_of() {
	json=$(printf '%s\n' "$1" | tr -d ' \t\r\n')
	printf '%s\n' "$json" | grep -Eq '^\{"version":"[^"]*","commit":"[0-9a-f]{7,40}","date":"[0-9]{4}-[0-9]{2}-[0-9]{2}T[^"]*"\}$' || return 1
	beta_found=$(printf '%s\n' "$json" | sed 's/^{"version":"\([^"]*\)".*$/\1/')
	printf "%s\n" "$beta_found" | grep -Eq '^v?0\.([0-3]\.[0-9]+([-+][0-9A-Za-z.+-]*)?|4\.0-beta\.[0-9]+)$' || return 1
	printf "%s\n" "$beta_found"
}

# classify_cf FILE: what a cf says of itself, given PROBE_SECONDS to answer. Sets CF_KIND:
#   transition  it answers the probe: a transition cf of any release
#   beta        its `version --json` is that of a Code Flock beta (CF_BETA is the version)
#   foreign     anything else, a cf that does not run or does not answer in time included
classify_cf() {
	CF_KIND=foreign
	CF_BETA=
	run_limited "$PROBE_SECONDS" env CF_TRANSITION_PROBE=1 "$1" version --json >"$WORK/probe.out" 2>/dev/null </dev/null || return 0
	answer=$(cat "$WORK/probe.out")
	if [ "$answer" = "$TRANSITION_ANSWER" ]; then
		CF_KIND=transition
	elif CF_BETA=$(beta_version_of "$answer"); then
		CF_KIND=beta
	fi
}

# replace_cf DEST: the transition cf over DEST.
replace_cf() {
	place "$WORK/unpacked/cf" "$1" || return 1
	mv -f "$TMP_BIN" "$1" || return 1
	TMP_BIN=
}

# closed_to_others PATH: PATH belongs to the user running the installer or to root, and neither its group, nor
# others, nor an ACL entry can write it. A sticky directory such as /tmp counts as closed: others can add to it but
# not rename what is ours. Read from `ls -ldn` (the same on macOS and Linux): mode in field 1, owner id in field 3;
# only its first line counts, and a name with a newline is never closed. No metadata means not closed.
closed_to_others() {
	case $1 in *'
'*) return 1 ;; esac
	# shellcheck disable=SC2012 # the mode and owner of one path; find has no portable equivalent
	ls -ldn "$1" 2>/dev/null | awk -v me="$(id -u)" 'NR == 1 {
		seen = 1
		m = $1
		sticky = substr(m, 1, 1) == "d" && substr(m, 10, 1) == "t"
		writable = substr(m, 6, 1) == "w" || substr(m, 9, 1) == "w"
		ok = ($3 == me || $3 == 0) && (!writable || sticky)
	} END { exit !(seen && ok) }' || return 1
	acl_closed "$1"
}

# acl_closed PATH: no ACL entry lets anyone else write PATH. macOS lists its entries with `ls -lde` (an `allow` with
# a write-class right opens it; the `deny delete` of every home does not); elsewhere a `+` after the mode is a POSIX
# ACL, which counts as open.
acl_closed() {
	if [ "$(uname -s)" = Darwin ]; then
		# shellcheck disable=SC2012 # the ACL of one path; nothing else prints it on macOS
		if ls -lden "$1" 2>/dev/null | awk 'NR > 1' |
			grep -Eq ' allow .*(write|append|add_file|add_subdirectory|delete_child|chown)'; then
			return 1
		fi
	else
		# shellcheck disable=SC2012 # see closed_to_others
		ls -ldn "$1" 2>/dev/null | awk 'NR == 1 { exit substr($1, 11, 1) == "+" }'
	fi
}

# closed_path PATH: PATH and every directory above it are closed to others, so nobody else can swap what PATH
# names between the check and its use.
closed_path() {
	p=$1
	while :; do
		closed_to_others "$p" || return 1
		[ "$p" = / ] && return 0
		p=$(dirname "$p")
	done
}

# $DIR/cf exists and is not a directory.
transition_cf_in_dir() {
	if ! real=$(resolve_link "$DIR/cf"); then
		log "left $DIR/cf alone: it is a symlink that does not resolve to a file"
		return 0
	fi
	shown=$DIR/cf
	dest=$real
	if [ -L "$DIR/cf" ]; then
		shown=$real
		log "$DIR/cf is a symlink to $real"
	fi
	real_dir=$(cd -P "$DIR" && pwd -P)
	if [ "$real" = "$real_dir/cflk" ]; then
		log "$DIR/cf already runs cflk; nothing to change"
		return 0
	fi
	# Nothing is run, read or replaced until it is a regular file that only the user or root can change.
	if [ ! -f "$real" ]; then
		log "left $shown alone: it is not a regular file"
		return 0
	fi
	if ! closed_path "$real"; then
		log "left $shown alone: it, or a directory above it, is not owned by you or root, or can be written by others. Use cflk for Code Flock"
		return 0
	fi
	if cmp -s "$real" "$WORK/unpacked/cf"; then
		log "$shown is already the transition cf; nothing to change"
		return 0
	fi
	classify_cf "$real"
	case $CF_KIND in
		foreign)
			log "left $shown alone: it is not the cf of Code Flock (a different tool, one too new to be sure, or one that did not answer). Use cflk for Code Flock"
			return 0
			;;
		transition) done_message="updated the transition cf at $shown; it runs cflk and goes away in 0.5" ;;
		beta) done_message="replaced the cf of Code Flock $CF_BETA at $shown with the transition cf: it runs cflk and goes away in 0.5. Use cflk from now on" ;;
	esac
	# A link to another directory is replaced itself: a cf outside $DIR is never touched.
	if [ -L "$DIR/cf" ] && [ "$(dirname "$real")" != "$real_dir" ]; then
		dest=$DIR/cf
		done_message="replaced the cf link at $dest with the transition cf: it runs cflk and goes away in 0.5. Use cflk from now on"
	fi
	replace_cf "$dest" ||
		die 4 "cflk $VERSION is installed at $DIR/cflk, but $dest could not be replaced (no write access to $(dirname "$dest")?). cflk is installed; remove or replace $dest by hand (with sudo if its directory belongs to the system), or just use cflk"
	[ "$dest" = "$real" ] || log "$real was left alone; it is no longer reached through $dest"
	log "$done_message"
}

# The cf that a shell would run, when it is not the one handled in $DIR: said, never run or touched. A relative
# result (an empty or relative PATH entry) is not reported.
report_cf_on_path() {
	found=$(command -v cf 2>/dev/null) || return 0
	case $found in /*) ;; *) return 0 ;; esac
	[ -f "$found" ] || return 0
	[ "$(cd -P "$(dirname "$found")" && pwd -P)" != "$(cd -P "$DIR" && pwd -P)" ] || return 0
	if handled=$(resolve_link "$DIR/cf") && real=$(resolve_link "$found") && [ "$real" = "$handled" ]; then
		return 0
	fi
	log "another cf is first on PATH at $found; it was left alone. The Code Flock command is cflk; if that cf is an older Code Flock, remove it."
}

# After cflk is in place, the cf of Code Flock gets replaced by the transition cf. Anything else stays.
transition_cf() {
	if [ -d "$DIR/cf" ]; then
		log "left $DIR/cf alone: it is a directory (or a link to one). Code Flock is cflk"
	elif [ -e "$DIR/cf" ] || [ -L "$DIR/cf" ]; then
		transition_cf_in_dir
	fi
	report_cf_on_path
}

install_binaries() {
	DIR=${INSTALL_DIR%/}
	mkdir -p "$DIR" || die 4 "cannot create $DIR"
	[ ! -d "$DIR/cflk" ] || die 4 "$DIR/cflk is a directory (or a link to one); move it away and run the installer again"
	install_cflk
	transition_cf
	case ":$PATH:" in
		*":$DIR:"*) ;;
		*) log "$DIR is not on your PATH; add this line to your shell profile (~/.profile, ~/.zshrc): export PATH=\"$DIR:\$PATH\"" ;;
	esac
	log "next: 'cflk controller init' on the machine that will be the Controller, or 'cflk setup' to join an existing one"
}

main() {
	[ $# -eq 0 ] || die 2 "unexpected arguments: $* (configure with CFLK_VERSION and CFLK_INSTALL_DIR)"
	read_env
	BASE_URL=${RELEASE_BASE_OVERRIDE:-$DEFAULT_BASE_URL}
	BASE_URL=${BASE_URL%/}
	TMP_BIN=
	detect_platform
	require_commands
	WORK=$(mktemp -d) || die 4 'cannot create a temporary directory'
	trap 'rm -rf "$WORK"; [ -z "$TMP_BIN" ] || rm -f "$TMP_BIN"' EXIT
	write_trust_roots
	verify_the_verifier
	resolve_version
	download_tarball
	install_binaries
}

main "$@"

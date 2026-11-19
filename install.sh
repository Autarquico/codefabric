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
# Only after that does it look at an existing cf: the cf of a Code Flock beta is replaced by the transition cf
# (which runs cflk); any other cf is left alone.
# The trust root is the allowed_signers block below. While it holds the PLACEHOLDER no signature verifies,
# so the installer fails closed. Review-first users verify this file with install.sh.sig.
set -eu

DEFAULT_BASE_URL='https://github.com/Autarquico/codeflock/releases/download'
LATEST_URL='https://github.com/Autarquico/codeflock/releases/latest'
NAMESPACE='codefabric-release'
PRINCIPAL='release@codefabric'

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
	for c in curl tar awk sed grep mktemp ssh-keygen cp chmod mv mkdir rm cat wc tr basename dirname readlink cmp; do
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
		die 5 "$VERSION predates the cflk rename (its assets are cf_*, not cflk_*); install it with the install.sh published with that release: $BASE_URL/$VERSION/install.sh"
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
place() {
	dest_dir=$(dirname "$2")
	TMP_BIN=$(mktemp "$dest_dir/.$(basename "$2").XXXXXX") || die 4 "cannot write to $dest_dir"
	{ cp "$1" "$TMP_BIN" && chmod 755 "$TMP_BIN"; } || die 4 "cannot write to $dest_dir"
}

install_cflk() {
	place "$WORK/unpacked/cflk" "$DIR/cflk"
	"$TMP_BIN" version >"$WORK/cflk.version" 2>&1 </dev/null || die 4 'the new cflk does not run; nothing was installed'
	grep -qF "${VERSION#v}" "$WORK/cflk.version" || die 4 "the new cflk does not report $VERSION; nothing was installed"
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
	parent=$(cd "$(dirname "$p")" && pwd -P) || return 1
	printf '%s/%s\n' "$parent" "$(basename "$p")"
}

# The version of a cf that is without doubt the one of a Code Flock beta: `version --json` is exactly
# {"version","commit","date"} and the version is 0.4.0-beta.N or older. Prints it, or fails.
code_flock_beta_version() {
	json=$("$1" version --json 2>/dev/null </dev/null | tr -d ' \t\r\n') || return 1
	printf '%s\n' "$json" | grep -Eq '^\{"version":"[^"]*","commit":"[0-9a-f]{7,40}","date":"[0-9]{4}-[0-9]{2}-[0-9]{2}T[^"]*"\}$' || return 1
	found=$(printf '%s\n' "$json" | sed 's/^{"version":"\([^"]*\)".*$/\1/')
	printf '%s\n' "$found" | grep -Eq '^v?0\.([0-3]\.[0-9]+([-+][0-9A-Za-z.+-]*)?|4\.0-beta\.[0-9]+)$' || return 1
	printf '%s\n' "$found"
}

# After cflk is in place, the cf of Code Flock gets replaced by the transition cf. Anything else stays.
transition_cf() {
	[ -e "$DIR/cf" ] || [ -L "$DIR/cf" ] || return 0
	if ! real=$(resolve_link "$DIR/cf"); then
		log "left $DIR/cf alone: it is a symlink that does not resolve to a file"
		return 0
	fi
	shown=$DIR/cf
	if [ -L "$DIR/cf" ]; then
		shown=$real
		log "$DIR/cf is a symlink to $real; that file is the one examined and replaced"
	fi
	if [ "$real" = "$(cd "$DIR" && pwd -P)/cflk" ]; then
		log "$DIR/cf already runs cflk; nothing to change"
		return 0
	fi
	if cmp -s "$real" "$WORK/unpacked/cf"; then
		log "$shown is already the transition cf; nothing to change"
		return 0
	fi
	if ! old=$(code_flock_beta_version "$real"); then
		log "left $shown alone: it is not the cf of a Code Flock beta (a different tool, or too new to be sure). Use cflk for Code Flock"
		return 0
	fi
	place "$WORK/unpacked/cf" "$real"
	mv -f "$TMP_BIN" "$real" || die 4 "cflk is installed, but $real could not be replaced"
	TMP_BIN=
	log "replaced the cf of Code Flock $old at $shown with the transition cf: it runs cflk and goes away in 0.5. Use cflk from now on"
}

install_binaries() {
	DIR=${INSTALL_DIR%/}
	mkdir -p "$DIR" || die 4 "cannot create $DIR"
	[ ! -d "$DIR/cf" ] || die 4 "$DIR/cf is a directory (or a link to one); move it away and run the installer again"
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

#!/bin/sh
# Codefabric installer: curl -fsSL https://github.com/Autarquico/codefabric/releases/latest/download/install.sh | sh
#
# Env:  CF_VERSION=vX.Y.Z|latest   release to install (default latest, the newest non-prerelease)
#       CF_INSTALL_DIR             where cf goes (default ~/.local/bin)
#       CF_RELEASE_BASE_URL        testing/mirror override of the download base; never skips verification
# Exit: 2 invalid arguments, 3 unsupported platform, 4 prerequisite failure, 5 download/checksum/signature failure
#
# It verifies the signed checksum list, then the tarball, and only then installs the cf binary.
# The trust root is the allowed_signers block below. While it holds the PLACEHOLDER no signature verifies,
# so the installer fails closed. Review-first users verify this file with install.sh.sig.
set -eu

DEFAULT_BASE_URL='https://github.com/Autarquico/codefabric/releases/download'
LATEST_URL='https://github.com/Autarquico/codefabric/releases/latest'
NAMESPACE='codefabric-release'
PRINCIPAL='release@codefabric'

log() { printf 'install.sh: %s\n' "$*" >&2; }
die() { code=$1; shift; log "error: $*"; exit "$code"; }

write_trust_roots() {
	cat >"$WORK/allowed_signers" <<'TRUST'
# BEGIN allowed_signers
release@codefabric namespaces="codefabric-release" ssh-ed25519 PLACEHOLDER
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
	[ "$OS-$ARCH" != darwin-amd64 ] || die 3 'unsupported platform: darwin/amd64 (releases cover linux amd64/arm64 and darwin arm64)'
}

require_commands() {
	for c in curl tar awk sed grep mktemp ssh-keygen cp chmod mv mkdir rm cat wc tr basename; do
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

fetch() { # url destination
	if [ -n "${CF_RELEASE_BASE_URL:-}" ]; then
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
	if [ -n "${CF_RELEASE_BASE_URL:-}" ]; then
		url="${CF_RELEASE_BASE_URL%/}/latest"
		final=$(curl -fsSL --retry 2 -o /dev/null -w '%{url_effective}' "$url") || die 5 "download failed: $url"
	else
		url=$LATEST_URL
		final=$(curl -fsSL --proto '=https' --tlsv1.2 --retry 2 -o /dev/null -w '%{url_effective}' "$url") || die 5 "download failed: $url"
	fi
	VERSION=${final##*/}
	is_version "$VERSION" || die 5 "could not resolve the latest release from $final"
}

resolve_version() {
	requested=${CF_VERSION:-latest}
	if [ "$requested" = latest ]; then
		latest_version
	else
		is_version "$requested" || die 2 "invalid CF_VERSION: $requested (vX.Y.Z or latest)"
		VERSION=$requested
	fi
	log "installing $VERSION"
}

# The signed list: line 1 "version: vX.Y.Z", then "<sha256>  cf_<ver>_<os>_<arch>.tar.gz" lines only.
check_checksums() {
	header=$(sed -n 1p "$WORK/checksums.txt")
	[ "$header" = "version: $VERSION" ] || die 5 "checksums.txt is signed for '${header#version: }', not $VERSION"
	if sed 1d "$WORK/checksums.txt" | grep -Evq '^[0-9a-f]{64}  cf_[0-9A-Za-z.+-]+_(linux|darwin)_(amd64|arm64)\.tar\.gz$'; then
		die 5 'checksums.txt contains a malformed line'
	fi
}

# Only the regular file `cf` leaves the tarball, and it lands in WORK, never in the install dir.
extract_cf() {
	tar -tzf "$1" >"$WORK/members" || die 5 "$(basename "$1") is not readable"
	if grep -Eq '(^/|(^|/)\.\.(/|$))' "$WORK/members"; then die 5 "$(basename "$1") has an unsafe member path"; fi
	[ "$(grep -cx 'cf' "$WORK/members")" -eq 1 ] || die 5 "$(basename "$1") does not contain exactly one cf"
	mkdir "$WORK/unpacked"
	tar -xzf "$1" -C "$WORK/unpacked" cf || die 5 "could not extract cf from $(basename "$1")"
	if [ ! -f "$WORK/unpacked/cf" ] || [ -L "$WORK/unpacked/cf" ]; then die 5 "cf in $(basename "$1") is not a regular file"; fi
}

download_tarball() {
	name="cf_${VERSION#v}_${OS}_${ARCH}.tar.gz"
	fetch "$BASE_URL/$VERSION/checksums.txt" "$WORK/checksums.txt"
	fetch "$BASE_URL/$VERSION/checksums.txt.sig" "$WORK/checksums.txt.sig"
	verify_signature "$WORK/checksums.txt" "$WORK/checksums.txt.sig" || die 5 'signature verification failed: checksums.txt'
	check_checksums
	expected=$(awk -v n="$name" 'NR > 1 && $2 == n { print $1; c++ } END { exit c != 1 }' "$WORK/checksums.txt") ||
		die 5 "$name is not listed exactly once in the signed checksums"
	fetch "$BASE_URL/$VERSION/$name" "$WORK/$name"
	[ "$(sha256_of "$WORK/$name")" = "$expected" ] || die 5 "checksum mismatch for $name"
	extract_cf "$WORK/$name"
}

install_cf() {
	dir=${CF_INSTALL_DIR:-$HOME/.local/bin}
	dir=${dir%/}
	mkdir -p "$dir" || die 4 "cannot create $dir"
	[ ! -d "$dir/cf" ] || die 4 "$dir/cf is a directory (or a link to one); move it away and run the installer again"
	TMP_BIN=$(mktemp "$dir/.cf.XXXXXX") || die 4 "cannot write to $dir"
	{ cp "$WORK/unpacked/cf" "$TMP_BIN" && chmod 755 "$TMP_BIN" && mv -f "$TMP_BIN" "$dir/cf"; } || die 4 "cannot install into $dir"
	TMP_BIN=
	log "installed $dir/cf ($VERSION)"
	case ":$PATH:" in
		*":$dir:"*) ;;
		*) log "$dir is not on your PATH; add this line to your shell profile (~/.profile, ~/.zshrc): export PATH=\"$dir:\$PATH\"" ;;
	esac
	log "next: 'cf controller init' on the machine that will be the Controller, or 'cf setup' to join an existing one"
}

main() {
	[ $# -eq 0 ] || die 2 "unexpected arguments: $* (configure with CF_VERSION and CF_INSTALL_DIR)"
	BASE_URL=${CF_RELEASE_BASE_URL:-$DEFAULT_BASE_URL}
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
	install_cf
}

main "$@"

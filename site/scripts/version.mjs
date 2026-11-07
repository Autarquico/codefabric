// The version the site is built for. It comes from SITE_VERSION (the release tag) and nowhere else.
export const RELEASE_TAG = /^v\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?$/;
export const INSTALL_URL = 'https://github.com/Autarquico/codefabric/releases/latest/download/install.sh';
export const UNRELEASED_NOTE = 'Unreleased build: no version is pinned. Replace vX.Y.Z with a release tag.';
export const PLACEHOLDER_VERSION = 'vX.Y.Z';

// Returns the release tag, or null for a build that is not tied to a release.
export function siteVersion(env = process.env) {
  const raw = (env.SITE_VERSION ?? '').trim();
  if (raw === '') return null;
  if (!RELEASE_TAG.test(raw)) throw new Error(`SITE_VERSION "${raw}" is not a release tag like v1.2.3`);
  return raw;
}

export function installCommands(version) {
  return {
    latest: `curl -fsSL ${INSTALL_URL} | sh`,
    pinned: `curl -fsSL ${INSTALL_URL} | CF_VERSION=${version ?? PLACEHOLDER_VERSION} sh`,
  };
}

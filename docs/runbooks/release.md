# Runbook: publicar una release

Spec: [release-installer-site](../specs/release-installer-site.md). Todo lo que sigue es manual salvo lo que
hacen `release.yml` y `pages.yml` una vez empujado el tag. Los pasos 1 a 4 bloquean el tag `v0.2.0`, no los PRs.

Convenciones fijas: principal `release@codefabric`, namespace `codefabric-release`, repo `Autarquico/codefabric`.

## 1. Generar la clave y custodiarla

```sh
cd "$(mktemp -d)"   # fuera del repo: que ningún git add pueda recoger la clave
ssh-keygen -t ed25519 -N '' -C release@codefabric -f codefabric-release
```

- Usa una máquina de confianza. El secret de GitHub necesita la clave sin passphrase. Para la copia de custodia,
  protégela con `ssh-keygen -p -f <copia>` antes de archivarla (o guárdala en el gestor de contraseñas) y borra
  los ficheros de trabajo al terminar.
- `codefabric-release` (privada) se guarda offline o en el gestor de contraseñas del owner. Es la única copia
  fuera del secret del paso 3. No va al repo, ni a otro secret, ni a otro workflow.
- `codefabric-release.pub` es pública. Se embebe en `install.sh` (paso 2).

## 2. Sustituir el placeholder de `install.sh`

Hasta este paso `install.sh` falla cerrado (sale con 5): la raíz de confianza es un placeholder.

En `install.sh`, entre los marcadores exactos:

```
# BEGIN allowed_signers
release@codefabric namespaces="codefabric-release" ssh-ed25519 PLACEHOLDER
# END allowed_signers
```

reemplaza la línea del medio por:

```
release@codefabric namespaces="codefabric-release" <tipo> <clave en base64>
```

con `<tipo> <clave>` = los dos primeros campos de `codefabric-release.pub` (`ssh-ed25519 AAAA...`). No
toques los marcadores ni el bloque `revoked_keys`. Abre un PR; CI (`installer`) debe quedar en verde y el
placeholder ya no debe existir (`grep -c 'ssh-ed25519 PLACEHOLDER' install.sh` da 0; el comentario de la
línea 10 de `install.sh` menciona la palabra PLACEHOLDER y no cuenta). Mergea antes de etiquetar: el tag
debe apuntar a un commit que ya lleve la clave real, porque `release.yml` verifica contra el `install.sh` de ese commit.

## 3. Environment `release` y su secret

En GitHub, Settings > Environments > New environment, nombre `release`:

- Required reviewers: el owner.
- Deployment branches and tags: regla de tag `v*` (y ninguna rama).
- Desactiva «Allow administrators to bypass configured protection rules» y crea un ruleset de tags `v*` que
  restrinja crear, actualizar y borrar al owner.
- Secret `CF_RELEASE_SIGNING_KEY`: contenido completo de la clave privada `codefabric-release`
  (con las líneas `-----BEGIN/END OPENSSH PRIVATE KEY-----`).

Solo el job `sign-publish` de `release.yml` referencia este environment y este secret; el job
`workflow-policy` de `ci.yml` lo comprueba.

## 4. Activar Pages

Settings > Pages > Source: **GitHub Actions**. `pages.yml` publica el sitio cuando `release` termina bien en un tag `v*`.

## 5. Hacer el tag

Con el commit ya en `main` y todos los checks requeridos de `ci.yml` en verde (`packaging/release/required-checks.txt`):

```sh
git switch main && git pull --ff-only
git tag vX.Y.Z
git push origin vX.Y.Z
```

Un tag con `-` (`v0.2.0-rc.1`) sale como prerelease. `latest` ignora prereleases y no hay fórmula para ellas.

`release.yml` hace, en orden:

1. `gates`: todos los checks de `required-checks.txt` deben estar en `success` en el commit del tag; si no, no sigue.
2. `build`: GoReleaser, sin secretos ni escritura; registra el digest de los tarballs.
3. `sign-publish` (pide la aprobación del owner en el environment `release`): comprueba que el tag está en
   `main` (`tag-in-main.sh`), que el artefacto coincide con el digest del build, escribe y firma
   `checksums.txt` e `install.sh` (`finalize.sh`), verifica contra la raíz embebida en `install.sh` (`check.sh`),
   crea la release en borrador, la verifica tal como quedó publicada (`verify-release.sh`) y solo entonces la publica.
   Si la verificación falla, borra el borrador.

Aprueba el despliegue solo tras comprobar que el tag y el commit son los esperados.

Después `pages.yml` construye y despliega el sitio con la versión del tag.

## 6. Verificar una release a mano

Con la release ya publicada:

```sh
curl -fsSL https://github.com/Autarquico/codefabric/releases/download/vX.Y.Z/install.sh | CF_VERSION=vX.Y.Z sh
```

Para revisar antes de ejecutar y comprobar las firmas sin instalar:

```sh
repo=/ruta/al/checkout-en-el-commit-del-tag   # raíz de confianza: nunca el install.sh descargado
d=$(mktemp -d) && cd "$d"
for f in install.sh install.sh.sig checksums.txt checksums.txt.sig; do
  curl -fsSLO "https://github.com/Autarquico/codefabric/releases/download/vX.Y.Z/$f"
done
"$repo/packaging/release/verify-asset.sh" checksums.txt "$repo/install.sh"
"$repo/packaging/release/verify-asset.sh" install.sh "$repo/install.sh"
```

El conjunto completo se comprueba con `"$repo/packaging/release/verify-release.sh" vX.Y.Z "$repo/install.sh"`
(necesita `gh`; el segundo argumento es el `install.sh` del repo, no el descargado).
Salida 5 en cualquiera de ellos = firma o checksum inválidos: no instales.

## 7. Publicar la fórmula de Homebrew

CI no tiene token del tap; es un paso humano. Si `autarquico/homebrew-tap` no existe, créalo (repo público con
una carpeta `Formula/`).

```sh
# en un checkout del repo, en el commit del tag
gh release download vX.Y.Z -p 'checksums.txt*' --dir "$d"
packaging/release/brew-formula.sh "$d/checksums.txt" install.sh vX.Y.Z > "$d/codefabric.rb"
```

`checksums.txt` y `checksums.txt.sig` deben estar juntos; `install.sh` es el del repo en el commit del tag
(nunca el de la release; tras una revocación, el de `main`). El script
verifica la firma antes de leer nada y rechaza versiones con sufijo, plataformas que faltan o repetidas, y una
cabecera distinta de `vX.Y.Z`. No emite nada si falla.

Después, en el tap. `brew audit` e `install` resuelven la fórmula desde el tap instalado, así que se trabaja
dentro de ese checkout para probar el fichero recién copiado:

```sh
brew tap autarquico/tap
cd "$(brew --repository autarquico/tap)"
cp "$d/codefabric.rb" Formula/codefabric.rb
brew audit --strict --online autarquico/tap/codefabric
brew install autarquico/tap/codefabric && brew test autarquico/tap/codefabric
git add Formula/codefabric.rb && git commit -m "codefabric X.Y.Z" && git push
```

## 8. Rotación y revocación de la clave

- **Rotación** (sin sospecha de compromiso): genera la clave nueva (paso 1), añade su línea a
  `allowed_signers` en `install.sh` junto a la anterior, actualiza el secret y haz release. Cuando las
  versiones firmadas con la anterior ya no hagan falta, quita su línea.
- **Revocación** (clave comprometida): añade la clave pública, en una línea (`<tipo> <clave>`), dentro del
  bloque `# BEGIN revoked_keys` / `# END revoked_keys` de `install.sh` y retírala de `allowed_signers`. Mergea por
  PR, rota el secret de `release` y publica una release nueva. Todos los verificadores (`install.sh`,
  `check.sh`, `verify-asset.sh`, `brew-formula.sh`) pasan `-r revoked_keys`, así que lo firmado con esa clave deja de
  verificar con el `install.sh` nuevo. Un `install.sh` antiguo descargado antes no se entera: avisa a los usuarios. Revisa también la fórmula ya
  publicada en `autarquico/homebrew-tap`: vuelve a derivarla con `brew-formula.sh` usando el `install.sh` de `main`
  como TRUST; si falla, republícala desde la release nueva.
- `latest` no está firmado (la redirección de `releases/latest`): quien pueda alterarla puede servir una
  release anterior, firmada y válida. Se acepta hasta que haya canales firmados; fijar `CF_VERSION` lo evita.

## Trazabilidad: scenario a test o job

Los tests viven en `packaging/release/test/` (`make test-installer`; el job `installer` de `ci.yml` los ejecuta
e imprime `--- PASS: TestScenario_*`).

| Scenario de la spec | Tests (`TestScenario_...`) | Job |
|---------------------|----------------------------|-----|
| Instalación verificada | `VerifiedInstall`, `InstallTarget_*`, `LatestVersion_*`, `PortableAwk_*`, `VerifiedInstall_replacesPreviousBinary`, `_hintsWhenDirNotOnPath`, `_armLinux`, `_darwinArm64`, `_realBinary` (`installer_test.sh`) | `installer` (`ci.yml`) |
| Firma o checksum inválidos | `BadSignatureOrChecksum_tamperedChecksums`, `_signedByOtherKey`, `_archiveShaMismatch`, `_missingSignature`, `_revokedKey`, `_wrongPrincipal`, `_wrongNamespace`; endurecimiento en `Hardening_*` (`installer_test.sh`) | `installer` |
| Cabecera de otra versión | `HeaderOtherVersion` (`installer_test.sh`) | `installer` |
| Plataforma no soportada | `UnsupportedPlatform_darwinIntel`, `_freebsd`, `_unknownArch` (`installer_test.sh`) | `installer` |
| Placeholder sin clave | `Placeholder` (`installer_test.sh`) | `installer` |
| Puertas de la release | `ReleaseGates_*` (`gates_test.sh`: `require-checks.sh` y `tag-in-main.sh`) | `gates` y `sign-publish` (paso «tag must point into main») de `release.yml`; `installer` ejecuta los tests |
| Conjunto publicado | `PublishedSet_*` (`published_test.sh`: `check.sh` y `verify-release.sh`) | `sign-publish` de `release.yml` (`check.sh`, `verify-release.sh`); `installer` ejecuta los tests |
| Fórmula derivada | `FormulaDerivada_valid`, `_validWithoutExplicitVersion`, `_invalidSignature`, `_tamperedChecksums`, `_suffixedVersion`, `_suffixedRequestedVersion`, `_missingPlatform`, `_duplicatedPlatform`, `_invalidDigest`, `_headerVersionMismatch` (`brew_test.sh`) | `installer` |
| Sitio desde la release | `TestScenario_SitioDesdeLaRelease` (nombre de los jobs): tests unitarios de `site/scripts`, build, comandos de instalación con el tag y axe (serious/critical) sobre la landing y una página de docs | `site` de `site.yml` (en PRs) y `build` de `pages.yml` (en la release) |

Lo que no se ha ejercitado con una release real: la firma con la clave del owner, el environment `release` y el
despliegue de Pages. La cadena completa solo se ha probado con una clave efímera de tests.

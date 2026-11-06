---
codename: codefabric
title: "Instalador firmado, release y sitio"
type: task
status: draft
priority: P1
size: L
issue: null
prs: []
related_issues: []
depends_on: []
supersedes: []
related_specs: [codefabric-spec, binary-distribution-upgrade]
created: 2026-11-06
updated: 2026-11-06
---

# Instalador firmado, release y sitio

## Contexto / problema

La v0.2 es la primera release pública. Hoy no tiene cómo instalarse ni dónde explicarse:

- `release.yml` publica lo que da GoReleaser, sin firma y con el `checksums.txt` sin firmar.
- No hay instalador: hay que descargar el tarball y copiar `cf` a mano.
- No hay sitio. El README tiene 34 líneas.

El modelo es supermgmt (`../supermgmt`, ADR-0019 y ADR-0036). Se recorta según lo decidido el 2026-11-06:

- Pipeline firmado, sin canales.
- Landing y docs.
- La v0.2 espera a esto.
- Brief de diseño propio.

Esto no sustituye a [binary-distribution-upgrade](binary-distribution-upgrade.md), que trata `cf worker upgrade` y `/v1/binaries` (Fase 10). Esta spec cubre la primera instalación de una máquina.

## Scope

**Entra:**
- **Firma de releases con OpenSSH.** `ssh-keygen -Y sign`, namespace `codefabric-release`, principal `release@codefabric`.
  - Se firman `checksums.txt` e `install.sh`.
  - `checksums.txt` empieza por la línea `version: vX.Y.Z` y sigue con líneas `<sha256>  <archivo>`.
- **`release.yml` en tres jobs:**
  - `gates`: comprueba que todos los checks requeridos de `ci.yml` están en verde en el commit del tag.
  - `build`: GoReleaser, sin secretos ni permisos de escritura. Deja registrado el digest de lo que construye.
  - `sign-publish`: corre en el environment `release`, con el owner como revisor obligatorio. Antes de firmar comprueba que el tag está en main y que el digest coincide con el del build. Firma, verifica contra la raíz de confianza de `install.sh` y publica con `gh release create`. Un tag con `-` sale como prerelease.
- **`install.sh` en POSIX sh, para `curl -fsSL …/install.sh | sh`:**
  - Plataformas: linux amd64/arm64 y darwin arm64.
  - Lleva `allowed_signers` y `revoked_keys` embebidos, y una autoverificación de `ssh-keygen` (OpenSSH ≥ 8.2).
  - Resuelve `CF_VERSION` (vX.Y.Z, o `latest` a través de la redirección de GitHub). Verifica la firma de `checksums.txt`, que la cabecera sea de esa versión y el sha256 del archivo. Solo después instala `cf` de forma atómica en `CF_INSTALL_DIR` (por defecto `~/.local/bin`).
  - Al terminar dice si ese directorio está en el PATH y sugiere `cf setup` o `cf controller init`. No ejecuta nada por su cuenta.
  - Códigos de salida: 2 argumentos inválidos, 3 plataforma no soportada, 4 falta un prerrequisito, 5 fallo de descarga, checksum o firma.
- **Clave de prueba efímera en CI.** Se renderiza en una copia de test de `install.sh` y se ejercita la cadena completa: firmar, servir por HTTP local, instalar.
- **Raíz de confianza con placeholder.** El `install.sh` del repo lleva un placeholder que no verifica nada hasta que el owner genere la clave, así que la instalación falla cerrada.
- **Fórmula de Homebrew.**
  - `packaging/homebrew/codefabric.rb.tmpl` y `packaging/release/brew-formula.sh` la renderizan desde el `checksums.txt` firmado, verificando antes la firma. Solo para versiones vX.Y.Z sin sufijo.
  - Publicarla en `autarquico/homebrew-tap` es un paso humano.
- **Sitio en `site/`, con Astro + Starlight:**
  - Landing y docs: getting started, conceptos, CLI y TUI.
  - Las docs se generan desde `docs/`. Los frames de terminal salen de `internal/tui/testdata/*.golden`, nunca de mock-ups.
  - Se publica en GitHub Pages con `pages.yml` cuando `release` termina bien en un tag `v*`. El contenido sale del tag y el tooling de main.
  - Accesibilidad comprobada con axe y Playwright.
- **Brief de diseño** en `.claude/design-briefs/base.yml` y `landing.yml`:
  - Fraunces, IBM Plex Sans e IBM Plex Mono.
  - Acento «loom indigo» (`#4338ca` / `#818cf8`).
  - Iconos Tabler.
- **`docs/runbooks/release.md`:**
  - Generar la clave y custodiarla.
  - Crear el environment `release` y su secret.
  - Activar Pages.
  - Hacer el tag.
  - Publicar la fórmula.

**No entra:**
- Canales stable/beta firmados y su re-firma periódica, SBOM, provenance attestation, builds reproducibles y avisos de licencias de terceros. Quedan como followup.
- `cf worker upgrade`, `/v1/binaries` y auto-update ([binary-distribution-upgrade](binary-distribution-upgrade.md)).
- Paquetes de distribución (deb, rpm) y darwin/amd64.
- Que `install.sh` ejecute `cf setup`. Solo instala y sugiere.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`, o como un job de CI que lo nombra.

### Scenario: Instalación verificada
- **Given** una release firmada con la clave de prueba, servida por HTTP local
- **When** se ejecuta `install.sh` con `CF_VERSION=vX.Y.Z` y `CF_INSTALL_DIR` en un directorio vacío
- **Then** `cf` queda instalado allí, `cf version` dice `vX.Y.Z` y el instalador sale con 0

### Scenario: Firma o checksum inválidos
- **Given** una release con `checksums.txt` alterado, firmado con otra clave o con un archivo que no casa con su sha256
- **When** se ejecuta `install.sh`
- **Then** sale con 5, no escribe nada en `CF_INSTALL_DIR` y el error nombra el fichero

### Scenario: Cabecera de otra versión
- **Given** un `checksums.txt` firmado de la v0.2.0, servido en la ruta de la v0.2.1
- **When** se instala `CF_VERSION=v0.2.1`
- **Then** sale con 5 porque las checksums firmadas son de otra versión

### Scenario: Plataforma no soportada
- **Given** `uname` informa darwin/x86_64 o un sistema que no es Linux ni Darwin
- **When** se ejecuta `install.sh`
- **Then** sale con 3 antes de descargar nada

### Scenario: Placeholder sin clave
- **Given** el `install.sh` del repo con el placeholder de `allowed_signers`
- **When** se instala una release cualquiera
- **Then** sale con 5: falla cerrado

### Scenario: Puertas de la release
- **Given** un tag `v*` sobre un commit con algún check requerido que no está en verde, o un commit que no está en main
- **When** corre `release.yml`
- **Then** no se firma ni se publica nada

### Scenario: Conjunto publicado
- **Given** un tag válido aprobado en el environment `release`
- **When** termina `release.yml`
- **Then** la release contiene los tres tarballs, `checksums.txt`, `checksums.txt.sig`, `install.sh` e `install.sh.sig`, y `install.sh` verifica `checksums.txt.sig` con su raíz embebida

### Scenario: Fórmula derivada
- **Given** el `checksums.txt` firmado de una vX.Y.Z
- **When** se ejecuta `brew-formula.sh` con la raíz de confianza
- **Then** genera una fórmula con la url y el sha256 de cada plataforma. Una firma inválida, una versión con sufijo o una plataforma que falta o se repite la rechazan

### Scenario: Sitio desde la release
- **Given** una release vX.Y.Z publicada
- **When** corre `pages.yml`
- **Then** el sitio muestra el one-liner de instalación con esa versión y las docs de `docs/` en ese tag, los frames del TUI salen de sus goldens, y axe no encuentra violaciones serious ni critical en la landing ni en una página de docs

## Notas técnicas / riesgos

- **Referencia:** `../supermgmt` (`install.sh`, `packaging/release/{finalize,sign,check,verify-asset,require-checks,brew-formula}.sh`, `.github/workflows/{release,pages}.yml`, `site/`). Se copia el patrón, no el código entero: aquí no hay canales, SBOM ni modos de instalación.
- **`latest` sin canales:** se resuelve con la redirección de `releases/latest`, que no está firmada. Alguien capaz de alterar esa respuesta podría servir una release anterior, firmada y válida; el downgrade no se detecta. Se acepta hasta que haya canales firmados (followup). Fijar `CF_VERSION` lo evita.
- **Custodia de la clave:** solo la tiene el owner y solo vive como secret del environment `release`. Ningún otro workflow referencia ese environment. Hay un check de CI con actionlint y un grep sobre `.github/workflows/` que lo comprueba.
- **Acciones fijadas por SHA**, como en supermgmt.
- **Versión en el binario:** GoReleaser inyecta `{{.Version}}` sin la `v` (`internal/buildinfo`). El scenario "Instalación verificada" fija qué imprime `cf version`, y la implementación debe hacer que coincida.
- **El sitio no hace afirmaciones que no estén verificadas.** Lo que solo se probó contra fakeharness se dice así, con el tono de supermgmt.
- **Pasos humanos (bloquean el tag v0.2.0, no los PRs):**
  - generar la clave;
  - crear el environment `release` con el secret `CF_RELEASE_SIGNING_KEY` y el owner como revisor;
  - activar Pages con «GitHub Actions» como source;
  - crear `autarquico/homebrew-tap` si no existe.

## Tasks (no autoritativo — el board manda)

- [ ] A: firma, `finalize.sh`, `install.sh`, test de la cadena con clave efímera y `release.yml` con sus puertas.
- [ ] B: fórmula de Homebrew y runbook de release.
- [ ] C: brief de diseño, `site/` (landing y docs) y `pages.yml`.

## Referencias

- `docs/codefabric-spec.md` §41 (compatibilidad) y §43.1 (mapeo a releases).
- supermgmt ADR-0004 (firmas OpenSSH), ADR-0019 (pipeline e instalador) y ADR-0036 (Homebrew).

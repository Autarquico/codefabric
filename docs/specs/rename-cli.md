---
codename: codefabric
title: El CLI pasa a llamarse cof
type: task
status: active
priority: P1
size: L
issue: null
prs: []
related_issues: [195]
depends_on: []
supersedes: []
related_specs: []
created: 2026-11-15
updated: 2026-11-15
---

# El CLI pasa a llamarse cof

## Contexto / problema

El producto ya se llama Code Flock (repo `Autarquico/codeflock`), pero el binario sigue siendo `cf`,
que es el CLI de Cloud Foundry: instalar Code Flock en una máquina que lo tenga rompe uno de los dos. Por dentro sigue además el nombre anterior,
`codefabric`: módulo Go, label de launchd `com.codefabric.worker`, unidad `cf-worker`, directorios
`~/.local/share/codefabric` y `~/Library/Logs/codefabric`.

Decidido (2026-11-15): el binario pasa a ser `cof`.

## Scope

**Entra:**
- El binario, los comandos impresos, la ayuda, los mensajes y las sugerencias (`cf attach …` → `cof attach …`).
- Paquetes: el nombre de los tarballs, `install.sh`, la fórmula de Homebrew, `release.yml` y goreleaser.
- Servicios: el label de launchd y la unidad de systemd pasan a un nombre `codeflock`. `cof worker install-service`
  migra el servicio antiguo: lo para, lo quita y deja el nuevo arrancado, sin perder el enrolamiento ni el estado.
- Directorios de config, datos y logs con el nombre `codeflock`. La primera ejecución de `cof` los migra desde
  `codefabric` si existen y los nuevos no.
- Variables de entorno de configuración: `CF_HOME`, `CF_CONFIG_DIR`, `CF_DATA_DIR`, `CF_TOKEN` y
  `CF_VERSION` (install.sh) pasan a `COF_*`. El nombre `CF_*` se sigue leyendo con un aviso hasta la 0.5.
- Las skills de Claude Code y de Codex, las `allowed-tools` (`Bash(cof …)`), y la ruta y el marcador de lo que
  instala `cof claude install`. `cof claude install` actualiza lo instalado con `cf`.
- Docs, guía, README y site.
- Una transición: durante la 0.4 el paquete instala también un `cf` que avisa y ejecuta `cof`, salvo que ya
  exista otro `cf` en el PATH, que no se pisa.

**No entra:**
- Los códigos de error `CF_*` del wire (`CF_NOT_FOUND`, …). Son contrato de la API y del MCP, y se leen como
  "Code Flock". Se quedan.
- Las variables `CF_TEST_*` y `CF_REQUIRE_*` de los tests. Son internas y se renombran solo si sale gratis.
- El módulo Go `github.com/Autarquico/codefabric`. Renombrarlo es un cambio mecánico aparte, sin efecto para
  el usuario: va en su propio PR.

## Criterios de aceptación

### Scenario: Instalar junto a otro cf
- **Given** una máquina con el `cf` de Cloud Foundry en el PATH
- **When** se instala Code Flock con `install.sh` o con Homebrew
- **Then** queda `cof` instalado, el `cf` existente no se toca, y la instalación lo dice.

### Scenario: Actualizar un Worker de la 0.4.0-beta
- **Given** un Worker con el servicio `com.codefabric.worker` (launchd) o `cf-worker` (systemd) y sus directorios `codefabric`
- **When** se instala la versión nueva y se ejecuta `cof worker install-service`
- **Then** el servicio antiguo se para y se quita, el nuevo arranca con el mismo enrolamiento, Runs y journal,
  y `cof worker list` lo ve online con la versión nueva.

### Scenario: Cliente con la config antigua
- **Given** `~/.config/codefabric` con el enrolamiento del cliente
- **When** se ejecuta por primera vez cualquier comando de `cof`
- **Then** la config se migra a la ruta nueva, una sola vez, y el comando funciona sin volver a enrolarse.

### Scenario: Variable antigua
- **Given** `CF_HOME` definido y `COF_HOME` no
- **When** se ejecuta `cof`
- **Then** usa `CF_HOME` y avisa una vez en stderr de que pasa a `COF_HOME`. Si están las dos, gana `COF_HOME`.

### Scenario: El cf de transición
- **Given** la 0.4 instalada sin otro `cf` en el PATH
- **When** se ejecuta `cf ps`
- **Then** avisa en stderr de que el comando es `cof` y ejecuta `cof ps` con el mismo código de salida.

### Scenario: Skills
- **Given** las skills instaladas por `cf claude install`
- **When** se ejecuta `cof claude install`
- **Then** se actualizan a `cof`, con `allowed-tools` limitadas a los comandos `cof` exactos, y `doctor` no
  informa de nada ajeno.

### Scenario: Códigos de error estables
- **Given** un cliente de la API o del MCP que compara `CF_NOT_FOUND`
- **When** se actualiza a la versión con `cof`
- **Then** recibe los mismos códigos.

## Notas técnicas / riesgos

- Medido en main (6a2d8b9): unos 200 ficheros que no son Go contienen comandos `cf …`, y 931 ficheros Go
  contienen `codefabric`, en su mayoría por la ruta del módulo, que no entra aquí.
- Workers reales: omarch01 tiene la unidad systemd de usuario `cf-worker`; mac-mini tiene el LaunchDaemon del
  sistema `/Library/LaunchDaemons/com.codefabric.worker.plist` con `UserName`, y migrarlo exige sudo, así que
  la migración debe decirlo y no fallar a medias.
- Es un cambio incompatible para los scripts del usuario: la 0.4.0 estable sale ya con `cof`, y el `cf` de
  transición desaparece en la 0.5.
- El orden importa: primero los paquetes y el `cf` de transición, luego los servicios y los directorios, y al
  final docs y skills. Un solo PR grande se revisa mal: conviene partirlo.

## Tasks (no autoritativo — el board manda)

- [ ] T1 binario + ayuda + mensajes + `cf` de transición + paquetes (install.sh, brew, goreleaser, release.yml)
- [ ] T2 directorios y env `COF_*` con migración
- [ ] T3 servicios launchd/systemd con migración en `install-service`
- [ ] T4 skills, `cof claude install` / `cof codex install` y doctor
- [ ] T5 docs, guía, README, site
- [ ] (aparte) módulo Go → `github.com/Autarquico/codeflock`

## Referencias

- `docs/guide/getting-started.md` (instalación, servicio launchd)
- `internal/cli/claude.go` (skills instaladas)
- `packaging/release/`, `install.sh`

---
codename: codefabric
title: El CLI pasa a llamarse cof
type: task
status: active
priority: P1
size: L
issue: 195
prs: [196]
related_issues: [195]
depends_on: []
supersedes: []
related_specs: []
created: 2026-11-15
updated: 2026-11-15
---

# El CLI pasa a llamarse cof

## Contexto / problema

El producto ya se llama Code Flock (repo `Autarquico/codeflock`), pero el binario sigue siendo `cf`, que es el
CLI de Cloud Foundry: instalar Code Flock en una máquina que lo tenga rompe uno de los dos. Por dentro sigue además
el nombre anterior, `codefabric`, en servicios, directorios, la fórmula de Homebrew y el registro MCP.

Decidido (2026-11-15): el binario pasa a ser `cof`. Lo que el usuario ve y teclea cambia. Lo que está guardado
como contrato (códigos de error, keyring, config de repo, formato de backup) se queda, para no perder estado.

## Scope

**Entra:**
- **Binario y salida.** El binario se llama `cof`, y con él la ayuda, los mensajes y las sugerencias (`cof attach …`).
  `cof completion <shell>` genera la completion para `cof`, y la guía dice que hay que regenerarla.
- **Variables.**
  - Binario: `CF_CONFIG_DIR` y `CF_DATA_DIR` pasan a `COF_CONFIG_DIR` y `COF_DATA_DIR`.
  - `install.sh`: `CF_VERSION`, `CF_INSTALL_DIR` y `CF_RELEASE_BASE_URL` pasan a `COF_*`.
  - El nombre `CF_*` se sigue leyendo, con un aviso, hasta la 0.5. Si están los dos, gana `COF_*`.
  - Los nombres de secreto que empiezan por `COF_` se rechazan como ya se rechazan los `CF_`.
- **Directorios.** Config, datos y logs pasan de `codefabric` a `codeflock`. La migración es un `rename`, sin
  copia, y se rechaza si los dos directorios están en sistemas de ficheros distintos, diciendo qué mover a mano.
- **Servicios del Worker y del Controller.**
  - systemd: `cf-worker` y `cf-controller` pasan a `codeflock-worker` y `codeflock-controller`.
  - launchd: `com.codefabric.worker` y `com.codefabric.controller` pasan a `com.codeflock.worker` y `com.codeflock.controller`.
  - `cof worker install-service` y `cof controller install-service` son los únicos que migran los directorios en
    una máquina con servicio. Lo hacen en este orden: paran y quitan la unidad antigua, mueven los directorios,
    escriben la unidad nueva con las rutas `COF_*` y el mismo `TMUX_TMPDIR`, arrancan, y comprueban que el socket
    de tmux de los Runs no ha cambiado.
- **Paquetes.**
  - El tarball lleva `cof` y un `cf` de transición.
  - `install.sh` sustituye por el de transición un `cf` que sea de Code Flock, porque `cf version` lo identifica.
    Otro `cf` no lo toca, y lo dice.
  - La fórmula de Homebrew pasa a llamarse `codeflock` e instala solo `cof`, sin el de transición porque choca con
    `cf-cli`. `codefabric` queda como alias o como fórmula obsoleta que apunta a `codeflock`.
- **El `cf` de transición.** Ejecuta `cof` con los mismos argumentos, stdin y stdout, y devuelve el mismo código de
  salida. Avisa con una sola línea en stderr, y solo si stderr es una terminal: así `cf mcp serve` y los scripts
  quedan limpios. Desaparece en la 0.5.
- **Skills.** `cof claude install` y `cof codex install` escriben `Bash(cof …)` en `allowed-tools`, y el marcador
  pasa a `cof-version`. Una skill con el marcador antiguo `cf-version` cuenta como desactualizada y se actualiza
  sin `--force`.
- **MCP.**
  - `cof mcp install` registra el servidor como `cof` (`cof mcp serve`) y quita el registro `cf` si es de Code Flock.
  - La skill del MCP pasa de `codefabric` a `codeflock`.
  - `Implementation.Name` pasa a `codeflock`.
- **Documentación.** Docs, guía (incluido el LaunchDaemon de ejemplo), README y site.

**No entra (contratos guardados que se quedan):**
- Los códigos de error `CF_*` del wire y del MCP. Se leen como "Code Flock".
- El servicio `codefabric` del keyring: renombrarlo perdería los secretos guardados.
- `.codefabric/project.yaml` en los repos de los usuarios.
- El formato de backup `codefabric-backup/1`.
- Las variables de test `CF_TEST_*` y `CF_REQUIRE_*`.
- El módulo Go `github.com/Autarquico/codefabric`: es un cambio mecánico aparte, sin efecto para el usuario.

## Criterios de aceptación

### Scenario: Instalar junto al cf de Cloud Foundry
- **Given** una máquina con el `cf` de Cloud Foundry en el PATH
- **When** se instala Code Flock con `install.sh` o con Homebrew
- **Then** queda `cof` instalado, el `cf` existente no se toca, y la instalación lo dice.

### Scenario: Actualizar el cf de Code Flock
- **Given** un `cf` de Code Flock 0.4.0-beta instalado con `install.sh`
- **When** se ejecuta el `install.sh` nuevo
- **Then** se instala `cof`, el `cf` antiguo se sustituye por el de transición, y la salida lo dice.

### Scenario: Homebrew
- **Given** la fórmula `codefabric` instalada
- **When** se ejecuta `brew upgrade`
- **Then** se instala `codeflock` con `cof`, y el `cf` antiguo de la fórmula desaparece.

### Scenario: Actualizar un Worker con servicio de usuario
- **Given** un Worker con `cf-worker` (systemd) o `com.codefabric.worker` en LaunchAgents, sus directorios `codefabric` y Runs vivos
- **When** se instala la versión nueva y se ejecuta `cof worker install-service`
- **Then** el servicio antiguo se para y se quita, y los directorios se mueven
- **And** el nuevo arranca con el mismo enrolamiento, Runs, journal y secretos
- **And** los Runs vivos se adoptan
- **And** `cof worker list` lo ve online con la versión nueva.

### Scenario: Actualizar un Controller
- **Given** un Controller con `cf-controller` o `com.codefabric.controller`
- **When** se ejecuta `cof controller install-service`
- **Then** el servicio se migra igual, con la misma base de datos, PKI y relay
- **And** los clientes y los Workers siguen conectando sin volver a enrolarse.

### Scenario: Worker con un LaunchDaemon del sistema
- **Given** `/Library/LaunchDaemons/com.codefabric.worker.plist` instalado
- **When** se ejecuta `cof worker install-service` sin ser root
- **Then** no cambia nada, ni el servicio ni los directorios
- **And** imprime los comandos exactos con sudo (bootout del antiguo, mover los directorios, bootstrap del plist nuevo)
- **And** sale con error.

### Scenario: Cliente con la config antigua
- **Given** `~/.config/codefabric` con el enrolamiento del cliente, sin servicio en la máquina
- **When** se ejecuta por primera vez cualquier comando de `cof`
- **Then** la config se mueve a `~/.config/codeflock` y lo dice una vez
- **And** el comando funciona sin volver a enrolarse.

### Scenario: Servicio en marcha con los directorios antiguos
- **Given** una unidad instalada que apunta a los directorios `codefabric`
- **When** se ejecuta cualquier comando de `cof` que no sea `install-service`
- **Then** no se migra nada, y se avisa nombrando la unidad y el `install-service` que la migra.

### Scenario: Migración a medias
- **Given** uno de los dos directorios nuevos existe y el otro no
- **When** se ejecuta `cof`
- **Then** se migra solo el que falta, y lo dice.
- **Given** existen el antiguo y el nuevo del mismo tipo
- **Then** no se migra ninguno, y se avisa nombrando las dos rutas.
- **Given** el `rename` falla
- **Then** no se ha movido nada, y el error nombra el par de rutas.

### Scenario: Volver al cf antiguo
- **Given** los directorios ya migrados
- **When** se ejecuta el binario 0.4.0-beta antiguo
- **Then** no encuentra configuración y pide enrolarse. Esto es conocido: la guía de la versión dice que no se
  vuelve atrás sin deshacer el `rename`.

### Scenario: Variable antigua
- **Given** `CF_CONFIG_DIR` definido y `COF_CONFIG_DIR` no
- **When** se ejecuta `cof`
- **Then** usa `CF_CONFIG_DIR` y avisa una vez en stderr de que pasa a `COF_CONFIG_DIR`. Si están las dos, gana `COF_CONFIG_DIR`.

### Scenario: El cf de transición
- **Given** la 0.4 instalada con el `cf` de transición
- **When** se ejecuta `cf ps` en una terminal, y `cf mcp serve` con stderr redirigido
- **Then** `cf ps` avisa con una línea en stderr de que el comando es `cof`, y ejecuta `cof ps` con el mismo código de salida
- **And** `cf mcp serve` no escribe nada en stderr antes del protocolo.

### Scenario: Skills
- **Given** las skills instaladas por `cf claude install`, con el marcador `cf-version`
- **When** se ejecuta `cof claude install`
- **Then** se actualizan sin `--force`, con el marcador `cof-version` y `allowed-tools` limitadas a los comandos `cof` exactos
- **And** `doctor` no informa de nada ajeno.

### Scenario: MCP registrado con cf
- **Given** el servidor MCP `cf` registrado por Code Flock en Claude Code o Codex
- **When** se ejecuta `cof mcp install`
- **Then** queda registrado `cof` y el registro `cf` desaparece
- **And** un servidor `cf` que no es de Code Flock no se toca.

### Scenario: Contratos estables
- **Given** un cliente de la API o del MCP que compara `CF_NOT_FOUND`, secretos guardados en el keyring y un repo con `.codefabric/project.yaml`
- **When** se actualiza a la versión con `cof`
- **Then** recibe los mismos códigos, conserva los secretos y la config del repo se sigue leyendo.

## Notas técnicas / riesgos

- **Medido en main (6a2d8b9):** unos 200 ficheros que no son Go contienen comandos `cf …`. 931 ficheros Go
  contienen `codefabric`, en su mayoría por la ruta del módulo, que no entra aquí.
- **Variables que lee el código hoy:** `CF_CONFIG_DIR` y `CF_DATA_DIR` (`internal/config/paths.go:107,111`), y
  `CF_VERSION`, `CF_INSTALL_DIR` y `CF_RELEASE_BASE_URL` (`install.sh`). `CF_HOME` y `CF_TOKEN` no existen.
- **Nombres de servicio:** `internal/worker/daemon/service.go:164` y `internal/controller/daemon/service.go:27`.
  El backend launchd de svcunit solo gestiona LaunchAgents, y por eso el caso del LaunchDaemon solo informa.
- **Contratos que se quedan:** keyring `Service = "codefabric"` (`internal/adapters/secrets/keyring/keyring.go:27`)
  y `BackupFormat` (`internal/controller/daemon/backup.go:40`).
- **MCP:** `mcpServerName = "cf"` (`internal/cli/mcp_install.go:27`) e `Implementation.Name`
  (`internal/mcp/server.go:100`).
- **Homebrew:** `packaging/homebrew/codefabric.rb.tmpl` (`bin.install "cf"`). `install.sh` exige hoy un solo `cf`
  en el tarball.
- **Workers reales:** omarch01 usa la unidad systemd de usuario `cf-worker`. mac-mini usa un LaunchDaemon del
  sistema con `UserName` y necesita sudo.
- **Compatibilidad:** es un cambio incompatible para los scripts del usuario. La 0.4.0 estable sale ya con `cof`,
  y el `cf` de transición y la lectura de `CF_*` desaparecen en la 0.5.

## Tasks (no autoritativo — el board manda)

- [ ] T1: binario, ayuda, completion, mensajes, `cf` de transición, tarball, `install.sh` y release.
- [ ] T2: variables `COF_*` y directorios con migración. Incluye los casos de servicio en marcha y migración a
  medias, y la reserva del prefijo `COF_`.
- [ ] T3: servicios del Worker y del Controller en `install-service`, incluido el caso del LaunchDaemon.
- [ ] T4: skills, MCP y doctor.
- [ ] T5: Homebrew, docs, guía, README y site.
- [ ] Aparte: el módulo Go pasa a `github.com/Autarquico/codeflock`.

## Referencias

- `docs/guide/getting-started.md`: instalación y LaunchDaemon de ejemplo.
- `internal/cli/claude.go` y `internal/cli/mcp_install.go`.
- `internal/config/paths.go`, `internal/worker/daemon/service.go`, `internal/controller/daemon/service.go`.
- `packaging/release/`, `packaging/homebrew/`, `install.sh`.

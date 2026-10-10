# CodeFabric (`cf`) — Especificación técnica y de producto

**Estado:** Especificación de implementación — rev. 4.1 (2026-10-10; cambios en Apéndice B, decisiones en Apéndice C)  
**Objetivo:** Definir CodeFabric de principio a fin con suficiente precisión para implementar el sistema completo de forma incremental.  
**Interfaz principal:** `cf` (CLI + TUI)  
**Lenguaje recomendado:** Go  
**Principio rector:** *Las Tasks pertenecen a CodeFabric; los Runs ocurren en Workers.*

---

## 1. Visión

CodeFabric es un control plane local-first para ejecutar, mover, observar, comparar y coordinar coding agents sobre un pool dinámico de máquinas.

El usuario puede tener un MacBook, portátiles Linux, servidores Proxmox, VMs/LXC, VPS y workers efímeros. Cualquier máquina puede incorporarse o salir del pool. CodeFabric abstrae dónde se ejecuta el trabajo y permite tratar todo el compute disponible como un único fabric.

CodeFabric no reemplaza Claude Code, Codex CLI, OpenCode, Pi u otros harnesses. Los instala, configura, ejecuta y observa mediante adapters, conservando sus configuraciones nativas.

CodeFabric tampoco reemplaza a los proveedores de modelos. Anthropic, OpenAI, Google, OpenRouter u otros se modelan separadamente de los harnesses. CodeFabric monitoriza estado, cuota y consumo oficial cuando el proveedor lo permite, y atribuye internamente el uso observado a Projects, Tasks, Runs, Workers, Workflows y harnesses.

### 1.1 Objetivos de producto

1. Abrir `cf` desde cualquier máquina y ver el estado global del trabajo agentic.
2. Delegar una Task sin preocuparse inicialmente por el Worker.
3. Continuar una Task desde otra máquina mediante handoff/checkpoint, sin depender de la máquina donde comenzó.
4. Incorporar y retirar compute dinámicamente.
5. Adoptar configuraciones existentes de Claude/Codex/etc. y distribuirlas total o parcialmente.
6. Ejecutar experimentos reproducibles comparando modelos, harnesses, workflows y workers.
7. Exponer las mismas capacidades por CLI, TUI y MCP.
8. Mantener logs, eventos, artifacts y métricas suficientes para auditar y reproducir cada Run.
9. Mantener el execution plane operativo durante interrupciones temporales del Controller.
10. Ser local-first, portable y no introducir lock-in innecesario.

### 1.2 No objetivos iniciales

- Reimplementar Kubernetes/Nomad.
- Migración viva de procesos entre hosts.
- Filesystem distribuido generalista.
- Sustituir los formatos nativos de configuración de los harnesses.
- Crear un protocolo VPN propio; usar Tailscale/SSH inicialmente.
- HA/consenso multi-controller en v0.x.
- Multi-tenancy/RBAC empresarial en v0.x.
- Scheduler basado en ML en v0.x.
- UI web obligatoria.

### 1.3 Plataformas y requisitos no funcionales

Plataformas v0.x:

| Rol | Soportado | Notas |
|-----|-----------|-------|
| Controller | Linux amd64/arm64 | macOS sólo para desarrollo |
| Worker | Linux amd64/arm64, macOS arm64 | WSL2 cuenta como Linux |
| Cliente | Linux, macOS | Windows nativo fuera de v0.x |

Objetivos de escala (dimensionan SQLite single-writer y el diseño de streaming):

- ≤ 25 Workers, ≤ 100 Runs concurrentes, ≤ 10 clientes conectados;
- ≤ 1M events/mes con retention por defecto.

Objetivos de calidad:

- `cf status` / `cf ps`: < 300 ms p95 sobre la tailnet.
- Un evento emitido en un Worker aparece en el TUI en < 2 s p95 con Controller accesible.
- El journal del Worker sobrevive a un reinicio del daemon (fsync por lote, default cada 1 s o 64 entradas).
- El journal está acotado y degrada por etapas, sin prometer conservación ilimitada:
  1. al alcanzar `journal.max_size` (default 1 GB) se descarta raw output y después logs;
  2. si events + usage siguen creciendo, el usage se compacta en agregados por Run y minuto, y el Worker pasa a `degraded` (no acepta Runs nuevos);
  3. al alcanzar `journal.hard_max` (default 5 GB) se descartan los events más antiguos no críticos y se escribe un marcador `JournalTruncated` con el rango perdido, que el Controller muestra. Los events de cambio de estado de Run nunca se descartan.
- Sin HA: una caída del Controller no detiene Runs en curso (invariantes 3 y 11); su duración tolerable sin pérdida de observabilidad está acotada por el journal.

---

## 2. Invariantes

Estas reglas deben condicionar toda la implementación:

1. **Una Task nunca pertenece a un Worker; un Run sí.**
2. Un Worker puede desaparecer sin perder el estado global de la Task.
3. Un reinicio del Controller no debe matar Runs ya iniciados.
4. Todo Run debe registrar Project, Task, Worker, Workflow efectivo, harness y modelo cuando sean conocidos.
5. La configuración nativa permanece nativa; CodeFabric no impone un esquema universal que pierda semántica.
6. Configuración y secretos son dominios separados.
7. CLI, TUI y MCP llaman a las mismas operaciones del Core/API; no contienen lógica de negocio divergente.
8. Toda operación destructiva o sensible debe ser auditable.
9. Toda operación distribuida debe diseñarse como idempotente/reintentable.
10. Provider Usage y Fabric Attributed Usage son mediciones distintas y nunca deben presentarse como equivalentes.
11. Un fallo temporal del Controller no debe detener un proceso ya ejecutándose en un Worker.
12. Los Workers deben poder reconciliar su estado con el Controller después de una desconexión.
13. Ningún secret debe incorporarse accidentalmente a un Workflow versionado.
14. El usuario debe poder inspeccionar qué configuración efectiva recibirá un Worker antes de aplicarla.
15. El scheduler nunca debe asignar una Task a un Worker que no satisfaga sus capacidades/policies/requisitos.
16. El estado de una Task se **deriva** de sus Runs y de su completion policy (§11.7). Sólo `cancel`, `archive` y `complete` (completion manual) lo fijan explícitamente, y quedan registrados como marcadores persistentes, no como sobrescritura del estado.
17. La expiración de una lease nunca hace que un Worker mate un Run ya aceptado; sólo invalida asignaciones no aceptadas y reportes con epoch obsoleto (§10.4).
18. Un Run, una vez terminal, no vuelve a ejecutarse. Reanudar, mover o reintentar crea siempre un Run nuevo enlazado por lineage.

---

## 3. Terminología y modelo conceptual

### 3.1 Project

Agrupa un repositorio o unidad de trabajo persistente. Contiene defaults, políticas, Environment, evaluadores y Tasks.

Campos mínimos:

```yaml
id: project_...
name: backend
repo: git@host:org/backend.git
default_branch: main
default_workflow: personal
default_environment: backend
defaults:                      # heredados por Tasks/Runs (§3.3)
  mode: headless|interactive   # si falta: headless (§7.4)
  resources: {cpu: 2, memory_mb: 4096}
  completion: first_success
  retry: {on_failed: none, on_lost: ask, max_attempts: 1}
  budget: optional
repo_config: allow|deny        # §3.1, default allow
created_at: ...
```

Puede definir:

- repositorio y estrategia de checkout;
- Environment;
- workflow por defecto;
- evaluadores (`test`, `lint`, `build`, benchmarks);
- capabilities requeridas;
- secrets referenciados;
- policies de worker/modelo;
- instrucciones específicas del proyecto.

Fuente de la configuración del Project:

- El Controller guarda la configuración autoritativa.
- Opcionalmente, con `repo_config: allow`, el repo puede incluir `.codefabric/project.yaml`. Reglas normativas:
  - se lee del commit/snapshot inmutable del `source` del Run, nunca del worktree vivo;
  - sólo puede definir `evaluation`, `environment` y `defaults.resources`; cualquier otra clave → Run `failed` con `CF_REPO_CONFIG_INVALID` antes de ejecutar nada;
  - sobrescribe campo a campo la configuración del Controller, pero las **policies del Controller son restricciones que el repo no puede ampliar**: secret refs, policies, límites de recursos y capabilities sólo vienen del Controller;
  - los scripts que declare se ejecutan con el mismo aislamiento y los mismos secrets que el propio Run, nunca con más;
  - la configuración efectiva resultante se registra en el Run (digest) antes de ejecutar ningún script.
- `cf project explain <project> [--ref <ref>]` muestra la configuración efectiva y el origen de cada campo.

### 3.2 Plan

Unidad opcional de intención de alto nivel, especialmente útil para un master model vía MCP. Un Plan contiene Tasks y sus dependencias.

Ejemplo:

```text
Plan: Implement billing
├── Task: DB migration
├── Task: Backend API      depends_on: DB migration
├── Task: Frontend
├── Task: Integration      depends_on: Backend API, Frontend
└── Task: Review           depends_on: Integration
```

Campos:

```yaml
id: plan_...
project_id: project_...
name: implement-billing
state: draft|active|completed|failed|cancelled
failure_policy: stop|continue      # qué hacer con Tasks pendientes si una falla
defaults: {}                       # mismo formato que project.defaults; sobrescribe al Project
budget: optional                   # §15
created_by: human|mcp|automation
created_at: ...
```

Un Plan en `draft` no programa Runs. `active` permite al scheduler lanzar las Tasks cuyas dependencias estén satisfechas. El Plan se completa cuando todas sus Tasks están `completed`; falla cuando una Task falla y `failure_policy: stop`.

Las dependencias deben formar un DAG; el Controller rechaza ciclos (`CF_DEPENDENCY_CYCLE`). Una Task puede depender de Tasks de otro Plan del mismo Project, nunca de otro Project.

### 3.3 Task

Representa **qué se quiere conseguir**, independientemente de dónde se ejecute.

Campos:

```yaml
id: task_...
project_id: project_...
plan_id: optional
name: auth-race
prompt: Fix the authentication race condition
state: blocked|queued|running|waiting|completed|failed|cancelled|archived
waiting_reason: start|input|deferred|decision|review   # sólo si state=waiting
autostart: true              # false con --no-start
completed_manually_at: optional   # marcador de completion manual (§11.7)
priority: 0..N
depends_on: []
source:
  ref: main                  # base; default: project.default_branch
  snapshot: optional         # digest de snapshot dirty (§22.2)
mode: interactive|headless   # §7.4; default del Project
workflow: optional           # personal@17 | personal@latest; default del Project
environment: optional        # default del Project
requirements:
  harness: claude-code       # opcional; si falta, lo decide workflow/project
  model: optional
  capabilities: []
  resources: {cpu: 2, memory_mb: 4096}
  workers: {prefer: [], avoid: [], require: []}
completion: first_success|promote|manual   # §11.7
retry: {on_failed: none|reschedule|ask, on_lost: none|reschedule|ask, max_attempts: 1}
budget: optional
labels: {}
created_at: ...
created_by: human|mcp|automation
```

Una Task puede tener múltiples Runs, secuenciales o paralelos.

**Dos niveles de resolución:**

1. **Congelación lógica (al crear el Run, estado `queued`):** se resuelven todos los campos omitidos (Task → Plan → Project → defaults globales), `@latest` del Workflow, la versión del Environment y el commit de `source.ref`. El resultado es la spec del Run (`spec_digest`) y no cambia aunque el Run espere en cola. Todo lo que depende del contenido del repo (`.codefabric/project.yaml`, `digest_inputs` del Environment) se lee de ese commit congelado.
2. **Materialización (en el Worker, durante `preparing`):** Workflow + overlay del Worker + estado local no gestionado + Environment preparado. Depende del Worker, así que sus digests (`effective_config_digest`, `environment.digest`) los calcula y reporta el Worker al pasar a `running`.

Para reproducir o comparar Runs cuenta `spec_digest`; `effective_config_digest` explica diferencias entre Workers.

La `completion` se congela en la Task cuando se crea su primer Run; cambiarla después es una operación explícita (`cf task set <task> --completion …`).

### 3.4 Run

Un intento concreto de ejecutar una Task.

```yaml
id: run_...
short_id: run-42
task_id: task_...
worker_id: worker_...
attempt: 1
lease: {id: lease_..., epoch: 3}
lineage:
  kind: initial|continues|forked_from|retries
  parent_run_id: optional
  checkpoint_id: optional
mode: interactive|headless
workflow:
  id: personal
  version: 17
  digest: sha256:...
spec_digest: sha256:...               # configuración lógica congelada al crear el Run (§3.3)
config_key: sha256:...                # workflow digest + versión de overlay + config de harness del Project (§8.12)
effective_config_digest: sha256:...   # materialización real en el Worker; se registra en preparing
environment:
  id: backend
  version: 12
  digest: sha256:...
harness:
  id: claude-code
  version: ...
model:
  provider: anthropic
  id: ...
source:
  commit: ...
  dirty: false
  snapshot: optional
branch: cf/auth-race/run-42           # en el git relay (§22.4)
state: queued|preparing|running|waiting_input|checkpointing|suspended|completed|failed|cancelled|lost
end_reason: optional                  # exit_code, deferred, moved, drained, budget, lease_lost...
queued_reason: optional               # CF_NO_ELIGIBLE_WORKER, CF_BUDGET_EXCEEDED, dependency...
worker_reachable: true                # derivado, no persistido como estado (§11.3)
runtime: none|active|stopped          # none hasta que el Worker reporta el handle persistido; ver §10.7
started_at: ...
finished_at: ...
result_commit: optional
publication: none|pending|published|failed   # §22.6; none sólo si el harness nunca llegó a arrancar
evaluation: none|pending|passed|failed|error  # agregado de evaluators required (§13)
promoted: false
```

Un Run registra además usage, timing, artifacts, checkpoints, evaluaciones, logs y eventos.

`state` describe la **ejecución** del harness; `publication` y `evaluation` describen qué se ha hecho con su resultado. Un Run `completed` con `publication: pending` es un resultado todavía no disponible fuera del Worker. Los Runs no se archivan por separado: un Run está archivado cuando lo está su Task.

### 3.5 Worker

Máquina que aporta compute.

```yaml
id: worker_...
name: proxmox
hostname: ...
os: linux
arch: amd64
state: joining|online|degraded|draining|drained|offline|removed
daemon_version: ...
epoch: ...                    # ID de instancia del daemon; cambia en cada arranque
max_concurrent_runs: 4
resources:
  cpu_total: 16
  cpu_allocatable: 12
  memory_total_mb: 65536
  memory_allocatable_mb: 49152
capabilities:
  - docker
  - staging-network
availability_policy: always|idle|manual
```

Un Worker ejecuta `cf worker run` como daemon.

### 3.6 Workflow

Composición versionada y portable de configuraciones nativas de harnesses y herramientas, más reglas de materialización.

No es un formato universal de Claude/Codex. Contiene snapshots nativos, metadata y ownership.

### 3.7 Environment

Define el entorno reproducible del proyecto: runtimes, servicios y dependencias necesarios para ejecutar código.

Puede estar respaldado por Docker/devcontainers/Nix/mise/scripts, sin imponer uno inicialmente.

### 3.8 Harness

Programa que conduce el coding agent: Claude Code, Codex CLI, OpenCode, Pi, etc.

Un harness tiene adapter propio y configuración nativa propia.

### 3.9 Model Provider y Model

Separados del Harness.

Ejemplos de providers: Anthropic, OpenAI, Google, OpenRouter, proveedor local.  
Ejemplos de models: identificadores concretos devueltos/usados por el provider.

### 3.10 Artifact

Salida durable de un Run:

- commit/patch;
- archivos;
- reportes de tests;
- benchmark;
- screenshot;
- build;
- resumen/checkpoint;
- cualquier resultado declarado por el adapter/evaluator.

### 3.11 Checkpoint

Punto recuperable de un Run. Debe contener suficiente estado portable para reconstruir la continuación en otro Worker cuando el harness lo permita.

### 3.12 Event, Log y Output

- **Event:** hecho estructurado entendido por CodeFabric (`RunStarted`, `WorkerLost`, etc.).
- **Log:** mensaje estructurado de diagnóstico.
- **Output:** stdout/stderr/PTY bruto de procesos.

Se almacenan separadamente aunque el TUI pueda mostrarlos juntos.

### 3.13 Identificadores y referencias

- **ID canónico:** `<kind>_<ULID>` (`task_01J…`, `run_01J…`). Inmutable, ordenable por tiempo, preservado en backup/restore.
- **Short ID:** `<kind>-<n>` (`run-42`), secuencia monotónica global por tipo asignada por el Controller.
- **Name:** Projects, Workers, Workflows y Tasks tienen `name` slug. El de Task es único por Project entre Tasks no archivadas; si no se indica, se deriva del prompt.

Resolución de una referencia en CLI/TUI/MCP, en orden:

1. ID canónico exacto;
2. short ID;
3. name (acotado por `--project` o por el Project del cwd si hay ambigüedad);
4. prefijo único de ULID de ≥ 6 caracteres.

Si hay más de un candidato: `CF_AMBIGUOUS_REF` con la lista. Nunca se elige uno implícitamente.

---

## 4. Arquitectura

```text
                         Humans / Models
                              │
                 ┌────────────┼────────────┐
                 │            │            │
               cf CLI       cf TUI      CF MCP
                 └────────────┼────────────┘
                              │
                         Client SDK
                              │
                       CodeFabric API
                              │
                 ┌────────────▼────────────┐
                 │ CodeFabric Controller   │
                 │                         │
                 │ API / auth              │
                 │ Scheduler               │
                 │ Registry                │
                 │ Workflow service        │
                 │ Task/Run service        │
                 │ Model usage service     │
                 │ Event/log service       │
                 │ Artifact metadata       │
                 │ Reconciler              │
                 │ SQLite                  │
                 └────────────┬────────────┘
                              │ private network
              ┌───────────────┼────────────────┐
              ▼               ▼                ▼
          cf worker       cf worker        cf worker
           MacBook          Linux           Proxmox
              │               │                │
         Runtime adapter Runtime adapter Runtime adapter
              │               │                │
         Harness adapters / native configs / environments
              │               │                │
              └────────── Model APIs ──────────┘
```

### 4.1 Controller

Debe vivir preferentemente en un nodo permanente: LXC/VM en Proxmox, servidor doméstico o VPS.

Responsabilidades:

- API autoritativa;
- persistencia del estado global;
- registro/heartbeat de Workers;
- scheduler y cola;
- Projects/Plans/Tasks/Runs;
- Workflows/versiones;
- modelo de capabilities/policies;
- agregación de eventos/logs/usage;
- artifacts metadata;
- reconciliation;
- notificaciones;
- budgets;
- MCP server;
- backups/restore.

Inicialmente: un proceso Go + SQLite + filesystem/content store.

### 4.2 Worker daemon

Responsabilidades:

- registrarse y enviar heartbeat;
- anunciar hardware/capabilities;
- aceptar leases/Run assignments;
- preparar repo/worktree/environment;
- materializar Workflow efectivo;
- iniciar/observar/detener harnesses;
- gestionar runtime de sesión;
- capturar output/eventos/usage;
- crear checkpoints;
- buffer local durante pérdida del Controller;
- reconciliar al reconectar;
- reportar capacidad libre.

### 4.3 Cliente `cf`

Un único binario Go es preferible en v0.x.

Roles:

```bash
cf                         # TUI
cf <resource> <command>    # CLI
cf controller run          # daemon controller
cf worker run              # daemon worker
cf mcp serve               # si MCP no se sirve directamente desde controller
```

La distribución puede crear servicios systemd/launchd que invoquen el mismo binario.

---

## 5. Networking y transporte

### 5.1 v0.x

- Tailscale como red privada recomendada.
- SSH sobre Tailscale para bootstrap, diagnóstico y fallback.
- API Controller↔Worker autenticada sobre la tailnet.
- El usuario no debe necesitar conocer IPs en la UX normal.

### 5.2 Abstracción

```go
type Transport interface {
    Reachable(ctx context.Context, worker Worker) bool
    Exec(ctx context.Context, worker Worker, cmd Command) (Result, error)
    Copy(ctx context.Context, worker Worker, src, dst string) error
}
```

Implementación inicial `SSHTransport`; posteriormente RPC directo de `cf worker`.

### 5.3 Heartbeats

Cada Worker envía periódicamente:

- timestamp;
- resources libres;
- active runs;
- daemon version;
- capabilities;
- workflow state/digests relevantes;
- health.

Controller marca Worker `offline` tras TTL configurable (defaults en Apéndice A).

### 5.4 Modelo de conectividad

- **Workers y clientes inician siempre la conexión hacia el Controller.** Funciona tras NAT, con portátiles que duermen y sin abrir puertos en Workers.
- Las asignaciones llegan por long-poll/stream sobre `/v1/worker/assignments`; el Controller nunca necesita SSH a un Worker para operar.
- `Transport` (SSH) se usa sólo para bootstrap (`cf worker join` remoto), diagnóstico y `cf attach` en v0.x.
- `cf attach` v0.x: cliente → SSH → Worker → `Runtime.Attach`. Requiere que el cliente tenga acceso SSH al Worker; `cf doctor` lo verifica. Posterior: relay de PTY por el daemon del Worker a través del Controller, eliminando el requisito de SSH cliente→Worker.

---

## 6. Runtime de ejecución

### 6.1 Interfaz

```go
type Runtime interface {
    Start(ctx context.Context, spec RuntimeSpec) (RuntimeHandle, error)
    Stop(ctx context.Context, handle RuntimeHandle) error
    Attach(ctx context.Context, handle RuntimeHandle) error
    Status(ctx context.Context, handle RuntimeHandle) (RuntimeStatus, error)
    Logs(ctx context.Context, handle RuntimeHandle) (io.Reader, error)
}
```

### 6.2 Implementación inicial

`tmux` es una implementación pragmática para sesiones interactivas persistentes.

Debe ser detalle interno. El modelo de dominio nunca debe depender de nombres de sesiones tmux.

Runtimes futuros posibles:

- PTY directo;
- systemd-run;
- Docker;
- devcontainer;
- VM sandbox.

### 6.3 Attach

`cf attach <task|run>`:

1. resuelve Run activo;
2. resuelve Worker;
3. establece transporte;
4. delega en Runtime.Attach;
5. devuelve el terminal al usuario.

El usuario no especifica SSH/tmux normalmente.

---

### 6.4 Supervisión y estado local del Worker

Para que un Run sobreviva al reinicio del daemon (§9.6, invariante 3), el Worker no depende de su memoria:

- **Estado local durable** (`<data_dir>/state.db`, SQLite): por Run guarda lease + epoch, fase local, handle del Runtime, `result_commit` y cola de reportes pendientes de ACK.
- **Intención antes de efecto:** el Worker persiste `starting(run, epoch)` antes de llamar a `Runtime.Start`, y el handle después. `Runtime.Start` es idempotente por `(run_id, epoch)`: si ya existe una sesión con ese nombre derivado, la adopta en lugar de crear otra.
- **Wrapper de proceso:** el harness corre bajo `cf worker exec` dentro del Runtime. El wrapper escribe exit code, timestamps y el output capturado en `runs/<run-id>/` aunque el daemon no esté vivo.
- **Arranque del daemon:** antes de aceptar trabajo, recorre `state.db`, consulta cada handle al Runtime, recoge exit codes del wrapper y reconcilia con el Controller (§27.3). No acepta leases nuevas hasta terminar.

## 7. Harness adapters

### 7.1 Principio

**CodeFabric entiende Workers, Tasks y Runs. Los adapters entienden harnesses.**

### 7.2 Interfaz conceptual

```go
type HarnessProvider interface {
    ID() string
    Detect(ctx context.Context, host Host) (Detection, error)
    Install(ctx context.Context, host Host, opts InstallOptions) error
    Version(ctx context.Context, host Host) (string, error)

    SnapshotConfig(ctx context.Context, host Host, opts SnapshotOptions) (NativeSnapshot, error)
    ApplyConfig(ctx context.Context, host Host, snap NativeSnapshot, opts ApplyOptions) error
    DiffConfig(ctx context.Context, host Host, snap NativeSnapshot) (ConfigDiff, error)
    ValidateConfig(ctx context.Context, host Host) (Validation, error)

    Start(ctx context.Context, run RunSpec) (HarnessHandle, error)
    Resume(ctx context.Context, run RunSpec, cp Checkpoint) (HarnessHandle, error)
    Checkpoint(ctx context.Context, handle HarnessHandle) (Checkpoint, error)
    Stop(ctx context.Context, handle HarnessHandle) error

    Events(ctx context.Context, handle HarnessHandle) (<-chan HarnessEvent, error)
    SendInput(ctx context.Context, handle HarnessHandle, in Input) error

    Capabilities() HarnessCapabilities
    CollectUsage(ctx context.Context, handle HarnessHandle) ([]UsageEvent, error)
}

type HarnessCapabilities struct {
    Modes             []RunMode // interactive, headless
    Resume            bool      // reanudar sesión nativa
    NativeCheckpoint  bool      // exporta estado nativo portable
    NeedsInputSignal  bool      // detecta/declara espera de input
    StructuredInput   bool      // SendInput sin attach
    UsageEvents       bool
    ModelSelection    bool
    ConfigIsolation   bool      // puede leer su config desde un directorio por Run (§8.12)
}
```

Las capacidades reales pueden variar; los adapters deben declarar soporte y devolver `unsupported`, nunca inventar datos.

`Host` es el contexto de ejecución **local** (paths, env, exec) del proceso que invoca al adapter: el daemon del Worker para Runs y apply, el cliente `cf` para adopt. El Controller nunca ejecuta adapters sobre hosts remotos.

### 7.3 Harnesses iniciales

Prioridad:

1. Claude Code.
2. Codex CLI.
3. OpenCode.
4. Pi.

Cada adapter debe documentar:

- instalación;
- paths de config detectados;
- componentes adoptables;
- auth (siempre separada del workflow cuando sea secreto);
- start/resume semantics;
- formato de eventos disponible;
- obtención de modelo/usage;
- limitaciones de checkpoint/handoff.

### 7.4 Modos de ejecución

- `interactive`: el harness corre en un PTY gestionado por el Runtime; es attachable. El prompt inicial se entrega por el mecanismo nativo del harness (argumento o primer input).
- `headless`: ejecución no interactiva con salida estructurada (p. ej. stream JSON). No attachable; permite needs-input, usage y fin de Run fiables.

El adapter declara qué modos soporta. Default: `project.defaults.mode`; si no está definido, `headless`. El origen de la Task (CLI, TUI, MCP) nunca cambia el modo: la misma Task se comporta igual se cree desde donde se cree. `cf task new --interactive` / `cf do -i` lo fijan explícitamente. Pedir un modo no soportado → `CF_HARNESS_UNAVAILABLE` en elegibilidad, no en runtime.

Fin de un Run `interactive`: termina cuando termina el proceso del harness (exit code → `completed`/`failed`). Cerrar el attach nunca termina el Run.

---

## 8. Workflows y adopción de configuración nativa

### 8.1 `cf workflow adopt`

Debe detectar el entorno local y ofrecer adopción selectiva.

Ejemplo:

```text
Claude Code
  [x] settings
  [x] skills
  [x] commands
  [x] hooks
  [x] MCP configuration
  [x] instructions
  [ ] authentication

Codex
  [x] config
  [x] skills
  [x] MCP configuration
  [x] instructions
  [ ] authentication
```

### 8.2 Estructura conceptual

```text
workflows/personal/
├── manifest.yaml
├── claude-code/
│   ├── native/...
│   └── metadata.yaml
├── codex/
│   ├── native/...
│   └── metadata.yaml
├── opencode/...
├── pi/...
├── tools/...
└── environment/...
```

El contenido puede guardarse internamente en content-addressed storage aunque se exponga así conceptualmente.

### 8.3 Ownership por componente

Estados:

- `managed`: CodeFabric aplica/sincroniza.
- `local`: detectado pero nunca sobrescrito.
- `merged`: combina base gestionada y extensiones locales cuando el adapter puede hacerlo de forma segura.
- `ignored`: fuera del dominio de CodeFabric.

### 8.4 Effective configuration

```text
Effective Config = Workflow Version
                 + Worker Overlay
                 + Project Config
                 + Local Unmanaged State
```

El orden exacto y estrategia de merge debe ser declarada por cada adapter/componente.

### 8.5 Worker overlays

Permiten diferencias deliberadas:

```yaml
worker: linux
overlay:
  claude:
    hooks: local
  mcp:
    disable:
      - xcode
```

### 8.6 Operaciones

```bash
cf workflow list
cf workflow show personal
cf workflow adopt
cf workflow status
cf workflow diff [--worker X]
cf workflow commit
cf workflow apply personal@18 --worker proxmox
cf workflow sync
cf workflow rollback personal@17
cf workflow history personal
cf workflow explain claude-code --worker linux
```

### 8.7 Sync selectivo

Debe permitir:

- uno/muchos Workers;
- uno/muchos harnesses;
- componentes específicos;
- `--dry-run`;
- aplicar a Workers offline al reconectar opcionalmente.

### 8.8 Drift

`cf workflow status` compara digest/materialización actual contra la versión deseada.

Los cambios locales nunca deben importarse automáticamente a la fuente de verdad. El usuario debe adoptar/commit explícitamente.

### 8.9 Versionado

Cada versión tiene:

- versión humana incremental o ID;
- digest content-addressed;
- parent;
- autor/origen Worker;
- timestamp;
- changelog opcional;
- componentes modificados.

### 8.10 Secrets

Nunca forman parte del snapshot normal.

Los manifests sólo contienen referencias:

```yaml
env:
  GITHUB_TOKEN:
    secret: github.personal
```

La policy determina en qué Workers está disponible la referencia. Backend y entrega en §16.7.

### 8.11 Referencias y concurrencia de versiones

- Referencias: `personal@17` (versión), `personal@latest`, `personal@sha256:…` (digest).
- El número de versión lo asigna el Controller, monotónico por Workflow.
- `cf workflow adopt` y `commit` se ejecutan en el **cliente**: snapshot local → upload de blobs al Controller → commit con `parent`.
- Commit con concurrencia optimista: si `parent` ≠ head actual → `CF_WORKFLOW_CONFLICT`. El usuario resuelve con `cf workflow diff personal@head` y `cf workflow commit --rebase`. Nunca hay merge automático.
- Un Run resuelve `@latest` al crearse (punto único de congelación, §3.3) y registra versión + digest; cambios posteriores del Workflow no afectan a Runs en cola ni en curso.

---

### 8.12 Aislamiento de configuración entre Runs

Un Worker ejecuta varios Runs a la vez (§10.5); un `apply` o un Workflow distinto no puede alterar un Run activo.

- **Materialización por Run:** si el adapter declara `ConfigIsolation`, la configuración efectiva se materializa en `runs/<run-id>/config/` y el harness se lanza apuntando a ese directorio (p. ej. variable de entorno del directorio de config nativo). Cada Run tiene su propia copia inmutable; Runs con Workflows distintos conviven sin interferir.
- **Harness sin aislamiento:** su configuración es global del Worker. Entonces:
  - el Controller mantiene un **lock de configuración** por `(worker, harness)` con un `config_key` y un contador de titulares. Se toma en la misma transacción que crea la lease (§10.4) y cuenta leases pendientes y Runs activos; se suelta al liberar la reserva (§10.5). Sólo se asignan Runs de ese harness con el mismo `config_key` que el lock vigente;
  - en el Worker, aceptación, materialización y apply de un mismo harness se serializan con un mutex local por harness;
  - `workflow apply` sobre ese harness marca el lock como `apply_pending`: desde ese momento no se crean leases nuevas de ese harness en el Worker; el apply se ejecuta cuando el contador llega a 0 (o falla con `CF_WORKER_BUSY` si se pide `--no-wait`).
- La auth del harness (local, §16.7) se enlaza en el directorio del Run, no se copia.

## 9. Worker lifecycle

### 9.1 Join

Flujo recomendado:

```bash
cf worker join <controller>
```

Wizard:

1. autenticar con Controller;
2. detectar CPU/RAM/disco/OS/arch;
3. nombrar Worker;
4. elegir recursos aportados;
5. elegir availability policy;
6. detectar capabilities;
7. elegir Workflow inicial;
8. instalar harnesses/tools requeridos;
9. aplicar configuración;
10. solicitar autenticación local necesaria;
11. `doctor`;
12. registrar Worker ready.

### 9.2 Resource contribution

Configurable:

```yaml
resources:
  cpu_max: 8
  memory_max: 16GB
  disk_max: 100GB
availability: idle
```

### 9.3 Drain

```bash
cf worker drain macbook
```

Estados:

1. `draining`;
2. no acepta nuevos Runs;
3. Runs actuales terminan o se checkpoint/mueven según policy;
4. `drained` (el daemon sigue conectado pero sin trabajo); puede pasar a `offline` si se apaga.

Opciones:

```bash
cf worker drain macbook --wait     # default: espera a que terminen
cf worker drain macbook --move     # checkpoint + move de cada Run (§11.5)
cf worker drain macbook --force    # cancela Runs (requiere confirmación)
cf worker undrain macbook          # vuelve a online
cf worker remove macbook           # revoca credencial, estado removed (sólo sin Runs activos)
```

### 9.4 Doctor

`cf worker doctor` verifica:

- conectividad Controller;
- transporte;
- runtime;
- Git;
- disk;
- harnesses;
- workflow materializado;
- auth necesaria (sin revelar secrets);
- environments/capabilities.

Regla UX: **si `cf doctor` está verde, cualquier Task compatible puede ejecutarse en esa máquina.**

### 9.5 Máquina de estados del Worker

```text
joining ──doctor ok──▶ online ◀──▶ degraded
online|degraded ──drain──▶ draining ──sin Runs──▶ drained ──undrain──▶ online
cualquiera ──sin heartbeat > TTL──▶ offline ──heartbeat──▶ estado previo
cualquiera ──remove──▶ removed (terminal)
```

`degraded`: el Worker responde pero `doctor` falla en algo (disco, harness, versión incompatible). No recibe Runs que dependan del componente fallido.

### 9.6 Upgrade del binario

v0.x: instalación/actualización manual del binario. El Worker reporta su versión en cada heartbeat; si queda fuera de la policy de compatibilidad (§41), pasa a `degraded` y no recibe Runs. Reiniciar el daemon para actualizarlo no afecta a Runs en curso (§6.4).

Posterior (Fase 10): el Controller sirve binarios por os/arch y `cf worker upgrade [<worker>|--all]` los distribuye.

---

## 10. Scheduling

### 10.1 MVP

Scheduler determinista basado en filtros + scoring.

Filtro de elegibilidad:

- Worker online y no draining;
- recursos suficientes;
- capabilities requeridas;
- Environment soportado;
- harness instalado/aplicable;
- workflow disponible;
- secrets referenciados disponibles en el Worker y permitidos (§16.7);
- restricciones de aislamiento garantizadas por su Runtime (§16.8);
- harness sin `ConfigIsolation`: lock de configuración libre o con el mismo `config_key`, y sin `apply_pending` (§8.12);
- project/worker policies satisfechas.

Scoring inicial:

- local-first/preferred workers;
- recursos libres;
- carga actual;
- afinidad de repo/cache;
- prioridad de Task.

### 10.2 Estrategias futuras configurables

```text
fastest
cheapest
local-first
lowest-load
always-on
historical-best
preserve-model-limits
balanced
```

No requieren ML; pueden usar reglas y métricas históricas.

### 10.3 Queue

Tasks/Runs queued tienen prioridad. El scheduler debe respetar dependencias del DAG y límites de paralelismo.

### 10.4 Lease

La asignación Controller→Worker debe usar lease/assignment ID para evitar doble ejecución accidental tras retries.

Semántica:

1. **Asignación (Controller, una transacción SQLite):** crea la lease `{id, run_id, worker_id, epoch, accept_by}`, reserva los recursos en el Worker y emite `RunAssigned`. `epoch` es un fencing token monotónico por Run. El Run sigue `queued` (con lease pendiente).
2. **Aceptación (CAS en el Controller):** el Worker comprueba capacidad local, **persiste la asignación en `state.db`** (§6.4) y después llama a `accept(lease_id, epoch)`. El Controller la acepta sólo si la lease sigue vigente y el Run está `queued` con ese epoch; en la misma transacción pasa el Run a `preparing`. Aceptar dos veces la misma lease devuelve el mismo resultado. Si el Worker no tiene capacidad, la rechaza (`CF_WORKER_CAPACITY`) y la reserva se libera.
3. **El Worker no arranca nada hasta recibir la confirmación durable** de la aceptación. Si la respuesta se pierde, reintenta `accept` (idempotente): o recibe la confirmación, o `CF_STALE_LEASE` y descarta la asignación. Así no puede haber dos ejecuciones con leases distintas del mismo Run.
4. Si no se acepta antes de `accept_by` (default 60 s), la lease expira, la reserva se libera y el scheduler puede reasignar con `epoch + 1`.
5. Todo reporte del Worker sobre un Run lleva su `epoch`. Se distinguen tres casos:
   - **epoch vigente, Run no terminal:** se aplica normalmente;
   - **epoch vigente, Run terminal:** se aceptan los reportes de **finalización** (publicación, evaluación, artifacts, usage, logs, `RunStopped`) y la repetición idempotente del mismo reporte terminal cuyo ACK se perdió. Un cambio de estado de ejecución (p. ej. `running`) se rechaza con `CF_INVALID_TRANSITION`, y si indica que el proceso sigue vivo en un Run que el Controller terminó (cancel, lost) se ordena el stop de §10.7;
   - **epoch obsoleto:** se rechaza (`CF_STALE_LEASE`); el proceso es huérfano: el Controller ordena su stop (§10.7, con checkpoint si el harness y la fase lo soportan) y su worktree se conserva como artifact (`RunOrphaned`).
6. Una vez aceptada, la continuidad del Run **no** depende de renovar la lease (invariantes 11 y 17); la propiedad se confirma por heartbeats.
7. El crash del Worker entre aceptar y arrancar se cubre con §6.4: asignación persistida antes de `accept` + `Runtime.Start` idempotente por `(run_id, epoch)`.
8. **Asignación aceptada sin registro local** (p. ej. pérdida del disco del Worker): al reconectar (§27.3) el Controller envía las leases aceptadas de ese Worker. Para cada una sin registro local, el Worker busca la sesión del Runtime con el nombre derivado de `(run_id, epoch)`:
   - **no existe:** responde `assignment_unknown`; el Controller marca el Run `failed` (`end_reason: assignment_lost`) y aplica la retry policy. Como no llegó a arrancar nada, reintentarlo no duplica trabajo;
   - **existe:** reconstruye el registro en `state.db` con los datos de la lease que envió el Controller, adopta la sesión y lee lo que el wrapper haya dejado en `runs/<run-id>/` (exit code, output). Si el proceso sigue vivo, continúa supervisándolo; si ya salió, entra directamente en la finalización (§22.6). Si el worktree no existe, el Run termina `failed` (`end_reason: worktree_lost`) tras el stop de §10.7.

### 10.5 Recursos y concurrencia

- Cada Run reserva `requirements.resources` (default del Project). El scheduler contabiliza reservas, no consumo real.
- **Ciclo de una reserva:** se crea con la lease (paso 1) y se libera, en la misma transacción que el evento que lo permite, cuando:
  - la lease expira o es rechazada; o
  - el Run es terminal sin haber salido nunca de `queued` (ninguna lease aceptada); se libera en la misma transacción que la transición; o
  - el Run es terminal desde `preparing` y el Worker ha confirmado `RunStopped(never_started)` (§10.7); o
  - el Run es terminal **y** `runtime: stopped` (§10.7) **y** el sellado local ha terminado (con `result_commit` persistido o `publication: failed`) **y** no hay evaluación en curso.

  La publicación pendiente no retiene la reserva (sólo son pushes reintentados). El lock de configuración de §8.12 se suelta con la reserva. `drain` no libera reservas: sólo impide nuevas.
- El Worker aplica la misma regla a `max_concurrent_runs` y a su capacidad local: un Run cuenta hasta que su proceso está detenido, su sellado terminado y su evaluación acabada.
- El Worker vuelve a comprobar su capacidad local al aceptar; el Controller y el Worker pueden discrepar temporalmente, y en ese caso gana el rechazo del Worker.
- Un Worker acepta como máximo `max_concurrent_runs` y nunca reservas por encima de `*_allocatable`.
- Límites de concurrencia por harness/cuenta de provider: posteriores al MVP (Fase 9).

### 10.6 Determinismo y explicabilidad

- Empates de score se rompen por nombre de Worker; la misma entrada produce la misma decisión.
- Un Run sin Worker elegible permanece `queued` con `queued_reason: CF_NO_ELIGIBLE_WORKER`.
- `cf run explain <run>` muestra, por Worker, qué filtro de §10.1 falla o el score obtenido. Es la contrapartida de `cf doctor` desde el lado de la Task.


### 10.7 Stop de un Run

Todo Run cuyo harness llegó a arrancar termina con `runtime: stopped` confirmado, sea cual sea la causa. Tras un **exit normal**, el wrapper verifica que no queda ningún proceso de la sesión (mata los restos tras `stop.grace`) y el Worker reporta `RunStopped` igual que tras una orden de stop.

**Arranque frente a stop:** en el Worker, `starting` → `Runtime.Start` → persistir handle, y la ejecución de un stop, se serializan con un lock local por Run. Al procesar un stop:

- si el Run aún no ha llegado a `starting`, el Worker persiste `aborted` (a partir de ahí `Runtime.Start` se niega para ese Run) y reporta `RunStopped(never_started)`;
- si ya está en `starting` o tiene handle, ejecuta la parada normal descrita abajo.

`runtime: active` sólo se registra cuando el Worker reporta el handle ya persistido. Mientras un Run aceptado no tiene `RunStopped` confirmado, el Controller asume que puede estar arrancando y retiene la reserva.

Parar un proceso a petición es una orden durable con confirmación, independiente de checkpoints:

1. El Controller registra `stop(run_id, epoch, reason)` en la cola del Worker (persistida; se reentrega tras reconexión).
2. El Worker señala parada ordenada al grupo de procesos/sesión del Runtime, espera `stop.grace` (default 30 s), mata el grupo completo y verifica que no queda ningún proceso de la sesión.
3. El Worker reporta `RunStopped` (idempotente). El Controller marca `runtime: stopped` y libera la reserva si se cumple §10.5.
4. Tras la parada, el Worker sella y publica el worktree como en §22.6 (best-effort para Runs cancelados).

Se usa para `cancel`, para huérfanos (§10.4 punto 5), para `drain --force` y para `input_required` (§43, Fase 2). En este último caso la orden la origina el propio Worker y se persiste localmente antes de ejecutarla. El estado del Run (`cancelled`, `failed`) es inmediato; la capacidad no (§10.5). **El sellado nunca empieza antes de `RunStopped`.**
---

## 11. Task/Run lifecycle

### 11.1 Crear

```bash
cf task new "fix flaky auth tests" --project backend            # Task + primer Run
cf task new "fix flaky auth tests" --project backend --no-start # sólo Task
cf do "fix flaky auth tests"                                    # atajo de task new
```

`cf run` es el namespace del recurso Run (`cf run list|fork|compare|promote|…`) y no acepta un prompt como argumento: evita la ambigüedad entre `cf run "fork the parser"` y `cf run fork run-42`. Ver Apéndice C.

Si se omite `--project`, se infiere del repo del cwd (match por remote URL); si no hay match → error, nunca un default silencioso.

### 11.2 Estados Task

```text
blocked      dependencias no satisfechas
queued       esperando Worker
running
waiting      ver waiting_reason
completed
failed
cancelled
archived
```

### 11.3 Estados Run

```text
queued
preparing
running
waiting_input
checkpointing
suspended    terminal: detenido a propósito con checkpoint, para continuar en otro Run
completed
failed
cancelled
lost
```

El estado `unknown` de §27.2 no es un estado de Run: es el atributo derivado `worker_reachable: false` mientras el Run sigue en su último estado conocido.

### 11.4 Defer/resume

`defer`:

1. Run → `checkpointing` (se recuerda el estado previo).
2. **Quiesce:** si el adapter soporta checkpoint nativo, se le pide y se espera su confirmación de que el agente está detenido; si no, se detiene el harness (señal de parada ordenada, kill tras `checkpoint.grace`, default 30 s). Nunca se captura con el agente escribiendo.
3. Captura del worktree (§23.1) y push al git relay (§22.4).
4. Run → `suspended` (`end_reason: deferred`); Task → `waiting` (`waiting_reason: deferred`).

Si 2 o 3 fallan: `RunCheckpointFailed`. Si el harness sigue vivo, el Run vuelve a su estado previo y la operación se aborta; si ya se detuvo, el Run termina `failed` (`end_reason: checkpoint`) conservando el worktree local para reintentar la captura.

`resume`:

1. scheduler elige Worker o usa el indicado;
2. reconstruye source/environment/workflow;
3. aplica checkpoint/context;
4. crea **siempre** un Run nuevo con `lineage.kind: continues` (invariante 18).

### 11.5 Move/handoff

No es migración viva. Es una operación durable por etapas (`moves` en el Controller), reanudable tras un reinicio de cualquiera de las partes:

```text
requested
  → source_suspended    origen: defer completo (§11.4) → Run origen `suspended`
                        (u origen ya `lost`: se usa el último checkpoint/push)
  → target_created      Run nuevo `queued`, lineage continues, Worker destino fijado
  → target_started      el Run destino llega a `running`
  → done
```

Reglas:

- El Run destino **sólo se crea cuando el Run origen es terminal**. Como los reportes sobre un Run terminal se rechazan (§10.4 punto 5), el origen queda cercado aunque su Worker vuelva más tarde, sin depender del epoch del Run nuevo.
- Si la preparación del destino falla, el checkpoint se conserva, la Task queda `waiting(decision)` y `cf task move` o `cf task resume` pueden reintentar desde la misma etapa.
- Lineage: `run-10 --continued-by--> run-11`.

Casos límite:

- **Harness sin checkpoint nativo:** el move se hace con checkpoint mínimo (source + resumen mecánico). El CLI avisa (`CF_CHECKPOINT_UNSUPPORTED` como warning) y pide confirmación salvo `--yes`.
- **Worker origen offline:** el Run origen debe estar `lost` (o el usuario fuerza `lost` con confirmación). Sólo puede continuarse desde el último checkpoint o el último push al relay; el CLI muestra qué se perdería y exige confirmación. Si el Worker vuelve, su proceso es huérfano (§10.4 punto 5).

### 11.6 Needs input

Harness adapter detecta o declara necesidad de input. Run → `waiting_input`; Controller genera `TaskNeedsInput`. TUI inbox permite responder sin attach cuando el harness lo soporte.

```bash
cf task respond auth-race "use the existing mutex, don't add a new one"
```

Si el adapter no tiene `StructuredInput` → `CF_RUN_NOT_RESPONDABLE`, con la sugerencia de `cf attach`. La respuesta se registra como evento `InputProvided` con su actor.

### 11.7 Máquinas de estado

**Run:**

```text
queued ──lease aceptada──▶ preparing ──harness iniciado──▶ running
running ◀──▶ waiting_input
running|waiting_input ──defer/move/drain──▶ checkpointing
checkpointing ──captura ok──▶ suspended
checkpointing ──falla, harness vivo──▶ estado previo (running|waiting_input)
checkpointing ──falla, harness detenido──▶ failed
running|waiting_input|checkpointing ──exit 0──▶ completed
running|waiting_input|checkpointing ──exit ≠ 0──▶ failed
preparing ──error──▶ failed
queued|preparing|running|waiting_input|checkpointing ──cancel──▶ cancelled   (el proceso se detiene según §10.7)
no terminal ──Worker no reconciliable tras run_lost_timeout──▶ lost
```

- Terminales: `suspended`, `completed`, `failed`, `cancelled`, `lost`. Cualquier otra transición devuelve `CF_INVALID_TRANSITION`.
- **Arbitraje:** el Controller aplica cada transición con compare-and-set sobre `(state, epoch)`; la primera gana y las demás reciben `CF_INVALID_TRANSITION`. Ejemplo: si el harness sale mientras se cancela, gana la que llegue primero; si sale durante un checkpoint, gana el exit (el checkpoint se descarta salvo que ya estuviera publicado).
- Cancel durante `checkpointing`: se aborta la captura; si ya se había publicado, se conserva como checkpoint del Run cancelado.

**Task (derivado; se aplica la primera fila que encaje):**

| # | Condición | Estado | `waiting_reason` |
|---|-----------|--------|------------------|
| 1 | marcador `archived` / `cancelled` (si hay ambos, gana `archived`) | `archived` / `cancelled` | — |
| 2 | se cumple la completion policy o hay marcador `completed_manually_at` | `completed` | — |
| 3 | alguna dependencia no está `completed` | `blocked` | — |
| 4 | algún Run en `preparing\|running\|checkpointing`, o `completed` con `evaluation: pending` o `publication: pending` | `running` | — |
| 5 | algún Run en `waiting_input` | `waiting` | `input` |
| 6 | algún Run `queued` | `queued` | — |
| 7 | sin Runs y `autostart: true` | `queued` (el scheduler crea el Run al cumplirse las dependencias) | — |
| 8 | sin Runs y `autostart: false` | `waiting` | `start` |
| 9 | último Run `suspended` | `waiting` | `deferred` |
| 10 | algún Run `completed` sin satisfacer la policy (evaluators `failed\|error`, `publication: failed`, o `completion: promote` sin promover) | `waiting` | `review` |
| 11 | último Run `failed\|lost` con retry `ask` | `waiting` | `decision` |
| 12 | último Run `failed\|lost` con retry `reschedule` y quedan intentos | `queued` (se crea el Run de retry) | — |
| 13 | último Run `cancelled` (sin cancelar la Task) | `waiting` | `decision` |
| 14 | último Run `failed\|lost` sin retry restante | `failed` | — |

"Último Run" es el de mayor short ID. La tabla es exhaustiva: cualquier combinación cae en alguna fila; un test unitario lo verifica.

**Completion policy:**

- `first_success` (default): el primer Run `completed` con `publication: published` y `evaluation: passed` (o sin evaluators required).
- `promote`: cuando un Run se promueve.
- `manual`: `cf task complete <task>`, que fija `completed_manually_at`.

La policy se congela con el primer Run (§3.3). Un fork posterior **no** cambia la policy: si la Task ya está `completed`, el fork se crea igualmente pero la Task sigue `completed`; para comparar y elegir, hay que crear la Task con `completion: promote` o cambiarla explícitamente antes del fork.

**Retry** (default `on_failed: none`, `on_lost: ask`): `reschedule` crea un Run nuevo con `lineage.kind: retries` desde el último checkpoint si existe, si no desde `source`, hasta `max_attempts`.

---

## 12. Fork, experimentación y comparación

### 12.1 Fork

```bash
cf run fork run-42 --workflow experimental@4 --model ... --worker auto
```

Crea otro Run de la misma Task (`lineage.kind: forked_from`). Punto de partida:

- `--from base` (default): el mismo `source` que el Run original. Es la única comparación justa entre combinaciones.
- `--from checkpoint:<cp>`: continuar desde un checkpoint concreto.
- `--from head`: el estado actual del worktree del Run original (push previo al relay).

### 12.2 Dimensiones comparables

- Workflow;
- harness;
- model/provider;
- Worker;
- Environment;
- versión del código;
- prompts/inputs;
- evaluators.

### 12.3 Reproducibilidad

Cada Run registra:

- source commit/digest;
- dirty state o snapshot;
- workflow digest;
- harness/version;
- model exacto cuando disponible;
- worker OS/arch/resources;
- Environment digest;
- inputs;
- evaluators;
- timestamps.

### 12.4 Compare

```bash
cf run compare run-41 run-43
```

Métricas:

- estado;
- duración;
- files/lines changed;
- tests/lint/typecheck/build;
- benchmarks;
- tool calls si observables;
- tokens/usage si observables;
- API cost si calculable;
- diff de Git trees;
- artifacts.

### 12.5 Promote

```bash
cf run promote run-43
```

Política configurable:

- merge/cherry-pick a branch objetivo;
- crear commit;
- crear PR mediante integración futura;
- requerir confirmación;
- ejecutar evaluators finales antes de promoción.

Default: `strategy: branch` — publica la rama del Run en `origin` como `cf/<task>`, sin merge, con confirmación. `merge` y `pr` son opt-in por Project. Sólo un Run por Task puede estar promovido; promover otro exige `--replace`.

Nunca promover automáticamente por score sin policy explícita.

---

## 13. Evaluators

Project define evaluadores reproducibles:

```yaml
evaluation:
  steps:
    - name: tests
      command: go test ./...
    - name: lint
      command: golangci-lint run
    - name: benchmark
      command: ./bench.sh
```

Resultado normalizado:

```yaml
name: tests
status: passed
exit_code: 0
duration_ms: 42100
metrics:
  passed: 243
  failed: 0
artifact_ids: []
```

Evaluators deben ejecutarse independientemente del harness para evitar que el agente sea juez único de su propio resultado.

Ejecución:

- En el mismo Worker y Environment del Run, sobre un worktree limpio en `result_commit` (no sobre el worktree vivo del agente).
- Momentos: `after_run` (default), `before_promote`, o manual con `cf run evaluate <run>`.
- Campos por step: `timeout` (default 15m), `required` (default `true`), `parse: none|junit|go-test-json|tap` para extraer `metrics`.
- Un timeout produce `status: error`, distinto de `failed`. Sólo `passed` cuenta para la completion policy.

---

## 14. Models: monitorización de APIs

### 14.1 Separación estricta

`Models` monitoriza APIs/providers, no harnesses.

```text
Harness: Claude Code / Codex / OpenCode / Pi
Model Provider: Anthropic / OpenAI / Google / OpenRouter / local
Model: model identifier concreto
```

### 14.2 Provider adapter

```go
type ModelProvider interface {
    ID() string
    Status(ctx context.Context) (ProviderStatus, error)
    Models(ctx context.Context) ([]Model, error)
    Usage(ctx context.Context) (ProviderUsage, error)
    Limits(ctx context.Context) (ProviderLimits, error)
    Billing(ctx context.Context) (ProviderBilling, error)
    Capabilities() ProviderCapabilities
}
```

Cada campo puede ser unsupported.

### 14.3 Provider-reported vs Fabric-attributed

Provider-reported:

- cuota;
- rate limits;
- reset;
- billing/credits;
- status;
- tokens si API lo ofrece.

Fabric-attributed:

- Runs;
- Project;
- Worker;
- Workflow;
- Harness;
- Task;
- timestamps;
- tokens/requests/cost observados por adapters.

### 14.4 Usage events

Formato interno:

```json
{
  "type": "model.usage",
  "run_id": "run_184",
  "provider": "anthropic",
  "model": "...",
  "input_tokens": 4213,
  "output_tokens": 812,
  "cache_read_tokens": 18342,
  "cache_write_tokens": 0,
  "request_count": 1,
  "latency_ms": 4200
}
```

El Controller deriva Project/Worker/Workflow/Harness desde `run_id`.

### 14.5 Drill-down

TUI/CLI deben permitir agregación por:

```text
provider → model → project → worker → workflow → harness → task → run → time
```

Ejemplos:

```bash
cf models
cf models usage --project backend
cf models usage --worker proxmox
cf models usage --workflow personal@27
cf models usage --since 7d
```

### 14.6 Coste

Sólo calcular coste monetario cuando exista pricing/configuración suficiente. Para subscripciones sin coste marginal atribuible, mostrar usage, no dólares inventados.

---

## 15. Budgets

Budgets aplicables a Plan/Task/Project:

```yaml
budget:
  max_runs: 20
  max_parallel: 4
  max_tokens: 5000000
  max_cost: 20.00
  max_duration: 6h
  warn_at: 0.8
  on_exceed: block_new|checkpoint_stop
  on_unobservable: warn|block
```

Todos los campos son opcionales; un límite ausente no limita.

El Controller, no el modelo, debe aplicar el límite.

Reglas:

- **Jerarquía:** el consumo de un Run cuenta en su Task, Plan y Project. Gana el límite más restrictivo.
- **Aviso:** `BudgetWarning` al alcanzar `warn_at` (default 80 %).
- **Exceso:** `on_exceed: block_new` (default) deja nuevos Runs en `queued` con `RunBlockedByBudget`; `checkpoint_stop` además suspende los Runs activos (§11.4). `max_duration` usa `checkpoint_stop` por defecto.
- **Métricas no observables:** si un Run no puede reportar tokens/coste (p. ej. suscripción sin usage), `on_unobservable: warn` (default) marca el budget como `partial` y lo muestra así; `block` impide lanzar ese Run.
- Subir un budget es una mutación auditada; con MCP requiere confirmación.

Eventos:

- `BudgetWarning`;
- `BudgetExceeded`;
- `RunBlockedByBudget`.

---

## 16. Policies y seguridad

### 16.1 Worker policy

```yaml
worker: vps
allow:
  - internet
  - github
deny:
  - home-lan
  - production
  - secret:personal
```

### 16.2 Run/harness policy

Puede restringir:

- filesystem scope;
- network;
- secret references;
- project access;
- commands/capabilities;
- maximum resources.

La enforcement real se apoya en mecanismos del OS/container, nunca sólo en prompts. Si el Runtime de un Worker no puede garantizar una restricción exigida, ese Worker no es elegible para el Run (§16.8).

### 16.3 MCP permissions

Perfiles sugeridos:

```text
read
execute
modify
admin
```

Ejemplo:

```yaml
mcp_master:
  profile: execute
  allow:
    - tasks.create
    - runs.start
    - runs.stop
    - runs.fork
    - runs.compare
  require_confirmation:
    - runs.promote
    - budgets.raise
    - workers.drain
    - workers.provision      # Fase 10
    - workers.destroy        # Fase 10
    - workflows.publish      # Fase 8+
  deny:
    - secrets.read           # no existe como tool; se lista para que ninguna policy pueda habilitarla
```

Los nombres de permiso son los mismos que los de las tools MCP (§17.2) y que las operaciones del Core; no hay un segundo vocabulario.

Perfiles:

| Perfil | Incluye |
|--------|---------|
| `read` | `*.list`, `*.get`, `logs.query`, `events.*`, `runs.compare`, `models.*` |
| `execute` | `read` + `tasks.create/cancel/respond`, `runs.start/stop/fork`, `plans.create` |
| `modify` | `execute` + `workers.drain`, `budgets.raise`; y, cuando existan, las tools mutantes de workflows (Fase 8+) |
| `admin` | todo; las acciones en `require_confirmation` siguen requiriéndola |

**Flujo de confirmación:**

1. La llamada devuelve `CF_CONFIRMATION_REQUIRED` con `confirmation_id`. La confirmación queda **vinculada de forma inmutable** a: operación, hash del payload, recursos afectados con su versión, actor solicitante y `request_id`.
2. Aparece en el Inbox (§19.5) mostrando exactamente esa operación; un humano responde con `cf approve <id>` / `cf deny <id>`.
3. **Aprobar ejecuta**: el Controller ejecuta la operación vinculada una sola vez, revalidando en ese momento permisos, policies, budgets y que las versiones de los recursos no hayan cambiado (si cambiaron → `CF_CONFIRMATION_STALE`, sin ejecutar).
4. El resultado se publica en `ConfirmationResolved` (approved + resultado / denied / expired / stale). El modelo puede esperarlo con `events.wait` o consultarlo con `confirmations.get`.
5. Repetir la llamada original con el mismo `request_id` devuelve el estado actual de la confirmación, no un `CF_CONFIRMATION_REQUIRED` cacheado. Las confirmaciones expiran (default 24 h).

### 16.4 Secret principle

Poder usar un secret no implica poder leerlo. Los workers/harnesses reciben el mínimo acceso necesario.

### 16.5 Audit

Registrar:

- actor (`human`, `mcp session`, `system`);
- acción;
- recurso;
- antes/después cuando corresponda;
- timestamp;
- resultado;
- confirmation ID para acciones sensibles.

### 16.6 Identidad y autenticación

v0.x asume un único propietario humano: todas las identidades humanas son el principal `owner`. No hay RBAC; sí hay identidades distintas por máquina y por sesión MCP, para auditar y revocar.

- **Controller:** `cf controller init` genera una CA propia (ECDSA P-256) y su clave **es** la identidad del Controller: no hay un keypair aparte, porque nada lo usaría. Toda la API va sobre TLS con esa CA, también dentro de la tailnet (Tailscale es red, no autenticación). CA y certificados hoja usan P-256 por compatibilidad con git/libcurl (ADR 0002, D2).
- **Primera credencial del owner:** `cf controller init` escribe en el host del Controller un credential bundle del owner (`0600`) y emite una invitación de cliente para la primera máquina. No hay otro camino de bootstrap: quien tiene shell en el host del Controller es el owner.
- **Invitaciones:** `cf controller token create --worker|--client --ttl 1h` emite un token de un solo uso con formato `cf://<host>:<port>/join?token=<t>#sha256:<fingerprint-CA>`. El cliente/Worker **verifica la CA contra el fingerprint de la invitación** antes de enviar nada (TOFU anclado al token, no a la red) y la fija para siempre.
- **Canje:** el Worker/cliente genera su clave y canjea el token por un certificado cliente (mTLS). El consumo del token es atómico (una transacción marca el token usado y crea la identidad); un segundo canje → `CF_UNAUTHENTICATED`.
- **Clientes adicionales:** también por aprobación desde un cliente ya conectado (`cf approve`). La credencial se guarda en el keychain del OS o en un fichero `0600`.
- **Revocación:** `cf worker remove` / `cf client remove` revocan la identidad; el Controller rechaza su certificado desde ese momento.
- **Sesiones MCP:** heredan la identidad del cliente que sirve MCP más un perfil de permisos (§16.3). Cada sesión tiene ID propio en audit.
- **Rotación:** certificados con caducidad (default 90 d). La renovación exige un certificado actual válido y no revocado; uno caducado requiere una invitación nueva.

### 16.7 Secrets

```go
type SecretStore interface {
    List(ctx context.Context) ([]SecretMetadata, error)                    // nunca valores
    Resolve(ctx context.Context, ref SecretRef, run RunID) ([]byte, error) // sólo Workers, sólo para un Run asignado
}
```

**v0.x: secrets locales al Worker.** El Controller sólo guarda **metadata**: qué refs existen, en qué Workers están disponibles y qué policies los permiten. Los valores viven en cada Worker, en un backend local:

- fichero `<config_dir>/secrets.yaml` con permisos `0600`, o
- keychain del OS (macOS Keychain, libsecret).

`cf worker secrets set <ref>` / `list` operan localmente en el Worker y sincronizan sólo la metadata. Un Run que referencia un secret no disponible en un Worker hace que ese Worker no sea elegible (`CF_SECRET_UNAVAILABLE` en `cf run explain`).

Reglas:

- **Autorización (desde Fase 2):** un Run sólo puede resolver refs que estén en el allowlist de su Project **y** no denegadas por la policy del Worker. Es la parte mínima del policy engine y no espera a Fase 9.
- Cada `Resolve` genera una entrada de audit en el Controller (ref, Run, Worker; nunca el valor).
- Entrega just-in-time: por env var o fichero `0600` en el directorio del Run; se borra al terminar el Run.
- Redacción best-effort de valores conocidos en logs y output capturado antes de persistirlos.
- La auth de los harnesses (logins OAuth de suscripción) permanece local en cada Worker y no es un secret gestionado salvo que se adopte explícitamente.

Posterior: un backend central cifrado en el Controller (o 1Password/Vault/sops) como otra implementación de `SecretStore`, una vez definidas gestión de clave, recuperación y distribución.

### 16.8 Autorización de Workers y modelo de confianza

**Modelo de confianza:**

- El **Worker** es de confianza para ejecutar el código de los Projects que se le asignan y para custodiar sus secrets locales. No es de confianza respecto a otros Projects, Runs ni Workers.
- El **agente** dentro de un Run no es de confianza: puede ejecutar cualquier cosa que su aislamiento permita.

**Alcance de un Worker autenticado** (el Controller lo comprueba en el servidor en cada llamada, nunca confía en IDs enviados por el Worker):

| Operación | Permitida sólo si |
|-----------|-------------------|
| reportar estado de ejecución de un Run | el Worker tiene la lease aceptada de ese Run con ese epoch y el Run no es terminal |
| reportar finalización (publicación, evaluación, usage, logs, `RunStopped`) | lease aceptada con ese epoch, aunque el Run sea terminal (§10.4 punto 5) |
| subir blobs | siempre content-addressed: el Controller verifica el digest; nunca sobrescribe un blob existente |
| asociar blobs a artifacts/checkpoints | lease aceptada de ese Run con ese epoch, aunque el Run sea terminal |
| push al relay | sólo a `refs/heads/cf/<task>/<run>` y `refs/cf/checkpoints/<cp>` de sus Runs; sin force-push sobre refs ajenas; nunca a otras ramas |
| fetch del relay | sólo Projects en los que tiene un Run asignado o ha tenido uno en la ventana de cache |
| `Resolve` de secrets | §16.7 |

**Aislamiento:** las restricciones de §16.2 son normativas, no best-effort. Cada Runtime declara qué puede garantizar (`fs_scope`, `network_deny`, `resource_limits`). Un Run cuya policy exige una restricción que el Runtime del Worker no garantiza **no es elegible** en ese Worker (filtro de §10.1). El Runtime `tmux` de v0.x no garantiza ninguna: sólo es elegible para Runs sin restricciones de aislamiento.

---

## 17. MCP de CodeFabric

### 17.1 Objetivo

Permitir que un master model controle CodeFabric desde una sesión compatible con MCP sin shell arbitraria.

### 17.2 Herramientas semánticas

Mínimo:

```text
projects.list
projects.get

plans.list
plans.get
plans.create

tasks.list
tasks.get
tasks.create
tasks.cancel
tasks.respond

runs.list
runs.get
runs.start
runs.stop
runs.fork
runs.compare
runs.promote
runs.logs

workers.list
workers.get
workers.capacity
workers.drain

workflows.list
workflows.get
workflows.diff

models.status
models.usage

logs.query
events.query
events.wait

artifacts.list
artifacts.get

budgets.get
budgets.raise

confirmations.get
```

Todas las tools mutantes aceptan `request_id` (idempotency key, §27.4).

No exponer inicialmente `exec arbitrary shell`.

### 17.3 Event-driven master

El master no debería hacer polling continuo. Diseñar subscriptions/notificaciones o una primitiva de espera/event stream para:

- RunCompleted;
- RunFailed;
- TaskNeedsInput;
- WorkerLost;
- BudgetExceeded.

El master puede quedar idle y reanudarse ante eventos.

Primitiva: `events.wait {types, filter, after_cursor, timeout}` bloquea hasta que llega un evento que cumple el filtro o vence el timeout (máx. 10 min), y devuelve los eventos más un `cursor`. Reanudar con el cursor garantiza que no se pierde ningún evento entre esperas mientras el cursor siga dentro de la retention de events. Un cursor más antiguo devuelve `CF_CURSOR_EXPIRED`; el cliente se resincroniza leyendo el estado actual (`tasks.list`, `runs.list`) y vuelve a esperar con un cursor nuevo (`events.wait` sin `after_cursor` devuelve el cursor actual).

### 17.4 Plan orchestration

MCP puede crear Plan + DAG de Tasks. CodeFabric sigue siendo responsable de scheduling, budgets, policies y ejecución determinista.

---

## 18. CLI

### 18.1 Principios

- comandos composables;
- salida humana por defecto;
- `--json` estable para scripts;
- exit codes útiles;
- `--dry-run` en operaciones mutantes complejas;
- todas las acciones TUI deben tener equivalente CLI.

### 18.2 Esqueleto

```text
cf
cf status
cf ps
cf doctor
cf do "<prompt>"          # atajo de cf task new
cf attach <task|run>
cf inbox
cf approve|deny <confirmation>
cf search <query>
cf setup | cf connect <controller>
cf gc | cf storage stats
cf restore <backup>
cf version | cf completion

cf project ...
cf plan ...
cf task ...
cf run ...
cf worker ...
cf workflow ...
cf models ...           # alias: cf model
cf logs ...
cf events ...
cf artifact ...
cf config ...
cf backup ...
cf controller ...
cf mcp ...
```

### 18.3 Ejemplos

```bash
cf do "fix auth race" --project backend
cf ps
cf attach auth-race
cf task defer auth-race
cf task resume auth-race
cf task move auth-race proxmox

cf run fork run-42 --workflow experimental@4
cf run compare run-42 run-43
cf run promote run-43

cf worker list
cf worker drain macbook
cf worker doctor linux

cf workflow adopt
cf workflow diff --worker linux
cf workflow sync

cf models
cf models usage --project backend --since 7d

cf logs --run run-184 --follow
cf logs --worker proxmox --level error --since 24h
```

---

## 19. TUI

### 19.1 Stack recomendado

- Bubble Tea;
- Bubbles;
- Lip Gloss;
- Cobra para CLI.

### 19.2 Regla

El TUI usa el mismo Client SDK/API que CLI y MCP. Nada de lógica de negocio exclusiva.

### 19.3 Home / Activity

```text
 CODEFABRIC                                      ● healthy

 TASKS          WORKERS          MODELS
 3 running      3/3 online       Anthropic 72%
 1 needs you                     OpenAI    31%

──────────────── Activity ───────────────────────────────
12:44  auth-refactor     tests failed
12:44  auth-refactor     agent analyzing failure
12:43  payments          ✓ completed
12:41  frontend          moved macbook → linux
12:38  linux             worker joined
12:32  personal@28       synced → proxmox
12:29  infra             needs your input
─────────────────────────────────────────────────────────
[t] Tasks [w] Workers [m] Models [r] Runs [f] Workflows
[p] Projects [a] Activity [l] Logs [/] Search
```

Los porcentajes de MODELS sólo se muestran cuando el provider reporta cuota (provider-reported). Si no, la columna muestra usage Fabric-attributed con su etiqueta, nunca un porcentaje estimado (invariante 10).

### 19.4 Vistas

#### Tasks

Columnas:

- name;
- project;
- state;
- active Run;
- harness/model;
- worker;
- duration;
- needs input.

Acciones:

- new;
- attach;
- respond;
- defer;
- resume;
- move;
- fork;
- cancel;
- logs;
- artifacts.

#### Workers

- online/offline/draining;
- CPU/RAM allocatable/used;
- active Runs;
- capabilities;
- workflow drift;
- health.

Acciones: drain, doctor, inspect, logs, workflow sync.

#### Models

Sólo providers/model APIs.

- provider status;
- models;
- quota/usage/reset cuando disponible;
- Fabric-attributed usage;
- drill-down Project/Worker/Workflow/Harness/Run/time.

#### Workflows

- versions;
- worker adoption state;
- drift;
- component ownership;
- diff;
- apply/sync/rollback/explain.

#### Runs

- current/history;
- compare;
- fork;
- promote;
- evaluators;
- usage;
- artifacts;
- logs.

#### Projects

- active Tasks;
- defaults;
- Environment;
- usage;
- evaluations;
- policies.

#### Logs

Filtros interactivos y follow.

### 19.5 Inbox / Needs You

Vista agregada de:

- Tasks waiting input;
- approvals;
- conflicts de workflow;
- budget warnings;
- failed Runs que requieren decisión.

Debe permitir responder a un harness sin attach cuando el adapter soporte input estructurado.

### 19.6 Command palette

`Ctrl+P` o equivalente: busca todas las acciones.

Modo `:` puede aceptar sintaxis CLI sin el prefijo `cf`, reutilizando parser/command descriptors cuando sea viable.

---

## 20. Logs, eventos y output

### 20.1 Eventos mínimos

```text
WorkerJoined
WorkerHeartbeat          # no se persiste en el event store; sólo worker_heartbeats
WorkerLost
WorkerReconnected
WorkerDraining
WorkerDrained
WorkerRemoved

PlanCreated
PlanStateChanged

TaskCreated
TaskStateChanged
TaskNeedsInput
InputProvided

RunQueued
RunBlockedByBudget
RunAssigned
RunStarted
RunCheckpointed
RunCheckpointFailed
RunWaitingInput
RunSuspended
RunCompleted
RunFailed
RunCancelled
RunLost
RunOrphaned              # proceso con epoch obsoleto detectado (§10.4)
RunForked
RunContinued
RunPromoted
RunPublished
RunPublicationFailed
JournalTruncated         # el Worker tuvo que descartar events (§1.3)

WorkflowAdopted
WorkflowCommitted
WorkflowConflict
WorkflowApplied
WorkflowDriftDetected

ModelUsageRecorded
ProviderStatusChanged
BudgetWarning
BudgetExceeded

ConfirmationRequested
ConfirmationResolved

ArtifactCreated
EvaluationStarted
EvaluationCompleted
```

### 20.2 Event schema

```go
type Event struct {
    ID          string
    SchemaVersion int
    Sequence    uint64     // orden global, asignado por el Controller al ingerir
    Timestamp   time.Time  // reloj del emisor
    ReceivedAt  time.Time  // reloj del Controller
    Source      string     // "controller" | "worker:<id>"
    WorkerEpoch *string
    WorkerSeq   *uint64    // monotónico por Worker+epoch; clave de deduplicación
    RequestID   *string
    Type        string
    Level       string
    Actor       ActorRef
    ProjectID   *string
    PlanID      *string
    TaskID      *string
    RunID       *string
    WorkerID    *string
    WorkflowID  *string
    Message     string
    Data        map[string]any
}
```

### 20.3 Logs CLI

```bash
cf logs
cf logs --follow
cf logs --worker proxmox
cf logs --project backend
cf logs --task auth-refactor
cf logs --run run-184
cf logs --level error
cf logs --since 2h
```

Filtros combinables.

### 20.4 Worker buffering

Cuando pierde Controller:

- mantener journal local append-only;
- sequence IDs monotónicos por Worker;
- persistir events/log chunks/usage;
- reintentar upload;
- Controller deduplica por `(worker_id, worker_epoch, worker_seq)`; el journal persiste epoch y seq, así que un reinicio del daemon no reutiliza claves.
- Timelines y listados ordenan por `Sequence`; `Timestamp` se muestra pero no ordena (los relojes de Workers pueden divergir).

### 20.5 Retention

Configurable:

```yaml
logs:
  retention:
    system: 30d
    events: 180d
    run_logs: 30d
    raw_output: 7d
  max_storage: 20GB
```

Artifacts/checkpoints tienen policies separadas.

---

## 21. Storage

### 21.1 Controller

MVP:

- SQLite WAL;
- filesystem para blobs/artifacts/workflow snapshots;
- content-addressed blobs por SHA-256;
- atomic writes;
- backup consistente.

### 21.2 Tablas conceptuales

```text
projects
plans
plan_tasks
tasks
task_dependencies
runs
run_lineage
workers
worker_heartbeats
worker_capabilities
workflows
workflow_versions
workflow_components
worker_workflow_state
artifacts
checkpoints
evaluations
events
logs_index
model_providers
model_usage_events
provider_usage_snapshots
budgets
policies
secrets_metadata
audit_entries
leases
confirmations
identities
environments
environment_versions
sequences
```

No es necesario que el schema físico coincida exactamente, pero estas relaciones deben existir.

### 21.3 Blob store

Usar paths por digest, por ejemplo:

```text
blobs/sha256/ab/cd/<digest>
```

Metadata en SQLite.

### 21.4 Evolución

Diseñar repository interfaces para poder mover metadata a Postgres o blobs a object storage sin cambiar dominio, pero no implementar antes de necesitarlo.

---

## 22. Source code y worktrees

### 22.1 Git como mecanismo base

Cada Run obtiene un worktree aislado o checkout equivalente.

Objetivos:

- runs paralelos sin pisarse;
- fork barato;
- diff/promote sencillo;
- checkpoint portable.

### 22.2 Dirty source

Al crear Task desde un workspace dirty:

- ofrecer snapshot/temporary commit/patch;
- registrar digest;
- nunca perder cambios locales.

### 22.3 Handoff

Sincronizar mediante Git cuando sea posible. Artifacts adicionales se transfieren por blob store/transport.

### 22.4 Git relay

Handoff, fork y checkpoints necesitan un remoto común que no dependa del Worker de origen ni ensucie `origin` con ramas de trabajo.

- El Controller mantiene un **bare repo por Project** (relay), servido por la misma API autenticada (git smart HTTP).
- Refs:
  - `refs/heads/cf/<task>/<run-short>`: rama de trabajo de cada Run;
  - `refs/cf/checkpoints/<checkpoint-id>`;
  - `refs/cf/snapshots/<digest>`: snapshots dirty (§22.2).
- Los Workers clonan de `origin` (con credenciales por secret ref) y usan el relay como segundo remoto. Cada Worker mantiene un clone bare por Project como cache y crea un worktree por Run.
- El agente commitea en su rama; el Worker hace push al relay al terminar (§22.6), en cada checkpoint y antes de move/defer.
- Nada llega a `origin` salvo por `promote` (§12.5).
- GC del relay: ramas de Runs de Tasks archivadas, según retention.
- **Alcance por fases:** Fase 1 implementa sólo push/fetch de `refs/heads/cf/*`, con la autorización de §16.8 y reintentos. Checkpoints y snapshots en el relay llegan con Fase 5; GC del relay con Fase 4.

### 22.5 Layout en el Worker

```text
<data_dir>/
├── repos/<project>.git          # clone bare compartido
├── worktrees/<run-id>/          # worktree aislado del Run
├── runs/<run-id>/               # secrets JIT, scratch, metadata local
└── journal/                     # §20.4
```

### 22.6 Publicación del resultado

El exit del harness y la disponibilidad de su resultado son pasos distintos:

1. **Exit y parada:** el wrapper (§6.4) registra el exit code y confirma la parada de toda la sesión (§10.7). Run → `completed`/`failed` según el exit y, **en la misma transacción**, `publication: pending`. Por eso `completed` con `publication: none` es imposible (invariante comprobado por test); `none` sólo existe en Runs que nunca arrancaron el harness.
2. **Sellado local:** el Worker commitea los cambios pendientes (tracked + untracked no ignorados, con las exclusiones de §23.1) como `cf: uncommitted changes at exit`, fija `result_commit` y lo persiste en `state.db` **antes** de reportar nada. Desde aquí el resultado sobrevive a reinicios del Worker.
3. **Publicación:** push de la rama al relay con reintentos y backoff (`publication: pending`). Se reintenta indefinidamente mientras el Controller esté caído; con el Controller accesible, tras `publication.max_attempts` (default 10) → `publication: failed` y `RunPublicationFailed`.
4. **Confirmación:** el Controller verifica que el ref existe en el relay y apunta a `result_commit` → `publication: published`, `RunPublished`.
5. **Evaluación** (§13): corre en el Worker sobre `result_commit`; puede ir en paralelo con la publicación.

**Fallo del sellado** (disco lleno, error de git, worktree corrupto): `publication: failed` con `RunPublicationFailed` y la causa; el worktree se conserva intacto y no entra en GC. `cf run publish <run>` reintenta sellado + push desde el principio. La Task queda `waiting(review)` (§11.7 fila 10).

Los reportes de los pasos 2–5 llegan después de que el Run sea terminal; §10.4 punto 5 y §16.8 los autorizan explícitamente.

Ninguna completion policy considera exitoso un Run sin `publication: published` (§11.7). El worktree local no se elimina por GC hasta que la publicación está confirmada.

---

## 23. Checkpoints

### 23.1 Contenido

Dependiendo del harness:

- source commit/patch;
- resumen estructurado del progreso;
- TODO siguiente;
- decisiones tomadas;
- comandos/tests ejecutados;
- references a artifacts;
- harness-native resume state si existe.

**Captura coherente:** sólo se captura con el agente detenido o tras la confirmación de quiesce del adapter (§11.4). Qué entra:

- ficheros tracked y untracked **no ignorados** del worktree, en un commit sobre `refs/cf/checkpoints/<cp>`;
- nunca ficheros ignorados por `.gitignore`, ni el directorio del Run (secrets JIT), ni ficheros que coincidan con `checkpoint.exclude` (default: `.env*`, `*.pem`, `*.key`); los excluidos se listan en el checkpoint para que el usuario sepa qué falta.

Si no se puede detener ni hacer quiesce del agente, no hay checkpoint: la operación falla con `CF_CHECKPOINT_UNSUPPORTED` en lugar de producir una captura incoherente.

Schema:

```yaml
id: cp_...
run_id: run_...
seq: 3
trigger: manual|move|defer|drain|interval|pre-destructive
source_ref: refs/cf/checkpoints/cp_...
summary:                 # markdown estructurado
  progress: ...
  next: ...
  decisions: ...
  commands: ...
native_state:            # opcional, blob opaco
  adapter: claude-code
  adapter_version: 1.4.0
  digest: sha256:...
artifact_ids: []
created_at: ...
```

- `native_state` sólo es restaurable por el mismo adapter con versión compatible (mismo major). Si no lo es, se usa sólo `source_ref` + `summary`.
- **Checkpoint mínimo** (harness sin soporte nativo): `source_ref` + resumen mecánico generado por CodeFabric a partir de commits, comandos y eventos del Run. Es posible siempre que el harness pueda detenerse, aunque pierda contexto del agente.

### 23.2 Automáticos

Crear antes de:

- move;
- defer;
- drain que requiera migración;
- operación destructiva;
- intervalos configurables opcionales.

### 23.3 Restore/fork

```bash
cf run checkpoint list run-42
cf run fork run-42 --from checkpoint:cp-3
```

---

## 24. Artifacts

Schema:

```yaml
id: artifact_...
run_id: run_...
type: patch|commit|file|test-report|benchmark|build|screenshot|other
name: ...
digest: sha256:...
size: ...
media_type: ...
retention: ...
```

CLI:

```bash
cf artifact list --run run-42
cf artifact get artifact_...
```

Artifacts pueden ser inputs de Runs posteriores.

---

## 25. Environments

### 25.1 Objetivo

Separar "cómo trabaja el agente" (Workflow) de "qué necesita el proyecto para ejecutarse" (Environment).

```text
Task
├── Workflow: personal@27
├── Environment: backend@12
└── Harness/Model
```

### 25.2 Provider interface

```go
type EnvironmentProvider interface {
    Detect(...)
    Prepare(...)
    StartServices(...)
    StopServices(...)
    Digest(...)
}
```

MVP puede empezar con `host` + scripts y Docker/devcontainer opcional.

### 25.3 Definición

```yaml
id: backend
version: 12
provider: host|docker|devcontainer|nix|mise
requires_capabilities: [docker]
prepare: ./scripts/cf-prepare.sh     # idempotente; se ejecuta en cada worktree nuevo
services:
  - name: postgres
    start: docker compose up -d db
    stop: docker compose stop db
    ready: pg_isready -h localhost
digest_inputs: [mise.toml, go.sum, docker-compose.yml]
```

- `Digest` se calcula sobre la definición + `digest_inputs`; se registra en el Run.
- `requires_capabilities` alimenta el filtro de elegibilidad del scheduler.
- Fallo de `prepare` → Run `failed` con `end_reason: environment`, sin arrancar el harness.

---

## 26. Notifications

Eventos notificables:

- TaskNeedsInput;
- RunCompleted;
- RunFailed;
- WorkerLost;
- BudgetExceeded;
- workflow conflict.

Canales iniciales:

- notificación nativa del cliente `cf` cuando está activo;
- hooks configurables.

Posteriormente providers: ntfy, Telegram, Slack, email, etc.

Debe existir deduplicación y preferencias por severidad/proyecto.

---

## 27. Reconciliation y tolerancia a fallos

### 27.1 Controller restart

Al arrancar:

1. cargar Runs no terminales y reiniciar sus temporizadores de `lost` (§27.2);
2. esperar/requerir heartbeats;
3. consultar Workers por runtime handles;
4. reconciliar estado;
5. reanudar operaciones asíncronas no terminadas (moves, confirmaciones aprobadas, applies) y órdenes de stop pendientes;
6. reanudar scheduler.

### 27.2 Worker disconnect

No marcar Run failed inmediatamente. El Run conserva su último estado con `worker_reachable: false` (§11.3):

- tras `worker_offline_ttl`: Worker `offline`;
- tras `run_lost_timeout` (default 30 min, deliberadamente largo para portátiles que duermen): Run `lost`. Se mide desde el último heartbeat recibido **con el Controller en marcha**: el tiempo que el Controller estuvo caído no cuenta (§27.1 reinicia los temporizadores);
- `lost` significa que el Controller **perdió la autoridad** sobre el Run, no que el proceso haya muerto; por eso el proceso, si reaparece, es huérfano y no se reutiliza;
- la retry policy de la Task (§11.7) decide `reschedule` o `ask`.

Si el Worker vuelve después con el proceso vivo de un Run `lost`, ese proceso es huérfano: el Controller ordena checkpoint + stop y conserva su rama en el relay como artifact (`RunOrphaned`). Nunca se "resucita" el Run (invariante 18).

### 27.3 Worker reconnect

Envía:

- daemon epoch/instance ID;
- active runtime handles;
- Runs con finalización pendiente (sellado, publicación, evaluación, stop);
- local event sequence;
- buffered events/logs;
- run states.

Controller deduplica y converge. Además responde con:

- las leases aceptadas de ese Worker, para detectar asignaciones sin registro local (§10.4 punto 8);
- las órdenes de stop pendientes (§10.7).

Hasta completar este intercambio el Worker no acepta leases nuevas.

### 27.4 Idempotencia

Mutaciones API aceptan `request_id`/idempotency key. Assignments usan lease ID (§10.4). Workflow apply usa desired digest.

Contrato de `request_id`:

- Alcance: `(principal, operación, request_id)`. Dos principals pueden usar el mismo ID sin colisión.
- El Controller persiste el hash del payload y la respuesta final durante `idempotency.ttl` (default 24 h).
- **Operaciones síncronas** (la gran mayoría): insertar la clave, aplicar la mutación, escribir sus eventos y guardar la respuesta ocurren en **una sola transacción**. Un crash deshace todo, y el reintento ejecuta limpio; nunca queda una clave sin resultado.
- **Operaciones asíncronas** (move, confirmaciones, apply de Workflow): la transacción inicial inserta la clave y crea un registro de operación durable; la respuesta es una referencia a esa operación. El Controller reanuda las operaciones no terminadas al arrancar (§27.1).
- Mismo ID + mismo payload → misma respuesta (o estado actual de la operación asíncrona), sin re-ejecutar.
- Mismo ID + payload distinto → `CF_IDEMPOTENCY_MISMATCH`.
- `CF_IN_PROGRESS` sólo puede darse con operaciones asíncronas cuyo registro aún no tiene estado consultable; es reintentable.
- `CF_CONFIRMATION_REQUIRED` no es una respuesta final: ver §16.3 punto 5.

Toda transición de estado, reserva, lease y su evento correspondiente se escriben en **una sola transacción** SQLite; no existe un estado en el que la tabla diga una cosa y el event store otra.

---

## 28. Backups, restore y portabilidad

### 28.1 `cf backup`

Debe producir backup consistente de:

- SQLite;
- workflow snapshots;
- metadata;
- policies/config;
- artifacts según policy;
- secrets **no incluidos por defecto**.

### 28.2 `cf restore`

Restaurar en otro Controller y conservar IDs/digests.

### 28.3 Controller migration

No necesario como comando v0.1, pero backup/restore debe permitir:

```text
Proxmox LXC → VPS
```

sin re-adoptar Workers/Workflows manualmente salvo reautenticación necesaria.

---

## 29. Garbage collection

### 29.1 Recursos

- worktrees;
- old clones;
- raw output;
- logs;
- expired checkpoints;
- unreferenced blobs;
- caches;
- archived Runs.

### 29.2 CLI

```bash
cf gc --dry-run
cf gc
cf storage stats
```

GC debe respetar referencias y retention policies. Nunca borrar artifact/checkpoint referenciado por Run no archivado sin policy explícita.

---

## 30. Worker efímero / cloud

Feature posterior.

Provider interface:

```go
type WorkerProvisioner interface {
    Plan(ctx context.Context, spec WorkerSpec) (ProvisionPlan, error)
    Create(ctx context.Context, spec WorkerSpec) (WorkerBootstrap, error)
    Destroy(ctx context.Context, id string) error
}
```

Flujo:

```text
queue pressure
→ provision temporary worker
→ bootstrap cf worker
→ apply workflow
→ execute
→ drain
→ destroy
```

Siempre sujeto a budget y confirmación/policy.

---

## 31. API del Controller

### 31.1 Estilo

Puede ser HTTP/JSON inicialmente. Mantener API versionada (`/v1`). Streaming mediante SSE/WebSocket según necesidad para events/logs/TUI.

### 31.2 Recursos mínimos

```text
/v1/status
/v1/projects
/v1/plans
/v1/tasks
/v1/runs
/v1/workers
/v1/workflows
/v1/models
/v1/events
/v1/logs
/v1/artifacts
/v1/evaluations
/v1/budgets
/v1/policies
```

### 31.3 Worker API

Separar endpoints autenticados de worker:

```text
/v1/worker/register
/v1/worker/heartbeat
/v1/worker/assignments
/v1/worker/events
/v1/worker/logs
/v1/worker/reconcile
```

### 31.4 Streaming

TUI necesita subscriptions a:

- global activity;
- Task/Run updates;
- worker health;
- logs;
- model usage.

---

## 32. Go package architecture

Estructura sugerida:

```text
cmd/cf/

internal/domain/
  project.go
  plan.go
  task.go
  run.go
  worker.go
  workflow.go
  model.go
  event.go
  artifact.go

internal/app/
  projects/
  tasks/
  runs/
  workers/
  workflows/
  scheduler/
  models/
  logs/
  reconciliation/

internal/adapters/
  harness/
    claude/
    codex/
    opencode/
    pi/
  runtime/
    tmux/
  transport/
    ssh/
  environment/
  modelprovider/
    anthropic/
    openai/
    google/
    openrouter/

internal/controller/
  api/
  store/
  blob/

internal/worker/
  daemon/
  journal/
  executor/

internal/client/

internal/tui/
internal/cli/
internal/mcp/
```

Regla: `domain` no importa adapters/UI/storage.

---

## 33. Interfaces de servicio conceptuales

```go
type WorkerService interface {
    List(context.Context) ([]Worker, error)
    Get(context.Context, WorkerID) (Worker, error)
    Join(context.Context, WorkerConfig) (Worker, error)
    Drain(context.Context, WorkerID, DrainOptions) error
}

type TaskService interface {
    Create(context.Context, TaskSpec) (Task, error)
    List(context.Context, TaskFilter) ([]Task, error)
    Defer(context.Context, TaskID) error
    Resume(context.Context, TaskID, ResumeOptions) (Run, error)
    Move(context.Context, TaskID, WorkerID) (Run, error)
}

type RunService interface {
    Start(context.Context, TaskID, RunOptions) (Run, error)
    Fork(context.Context, RunID, ForkOptions) (Run, error)
    Compare(context.Context, []RunID) (Comparison, error)
    Promote(context.Context, RunID, PromoteOptions) error
}

type WorkflowService interface {
    Adopt(context.Context, AdoptOptions) (WorkflowVersion, error)
    Diff(context.Context, WorkflowRef, Target) (WorkflowDiff, error)
    Apply(context.Context, WorkflowRef, []WorkerID, ApplyOptions) error
    Explain(context.Context, WorkflowRef, WorkerID, HarnessID) (EffectiveConfig, error)
}
```

---

## 34. Observabilidad interna

CodeFabric debe exponer su propia salud:

```bash
cf status
```

Incluye:

- Controller uptime/version;
- DB health;
- blob storage;
- workers online;
- queued/running/lost Runs;
- scheduler status;
- event backlog;
- worker journal backlog;
- storage usage.

Logs del sistema en formato estructurado JSON internamente; presentación humana en CLI/TUI.

---

## 35. Configuración de CodeFabric

Separar:

1. Controller config.
2. Worker config.
3. Client config.
4. Project config.
5. Workflow metadata.
6. Policies.

Paths deben seguir convenciones OS/XDG. `cf config paths` muestra ubicaciones efectivas.

Configuración debe soportar env vars para deployment, pero secretos no deben aparecer en `cf config show` sin redacción.

---

## 36. UX de setup

### 36.1 Primer Controller

```bash
cf controller init
cf controller install-service
```

Crea DB, identity y config; ofrece Tailscale binding y backup settings.

### 36.2 Primer workstation

```bash
cf connect <controller>
cf workflow adopt
cf worker join
```

También puede existir wizard:

```bash
cf setup
```

que detecta si la máquina será sólo client, worker, controller o combinación.

### 36.3 Nueva máquina

Objetivo UX:

```text
install cf
→ authenticate/connect
→ choose worker resources
→ choose workflow
→ install/sync harnesses
→ authenticate required providers locally
→ doctor
→ ready
```

---

## 37. Search

TUI `/` y CLI `cf search` deben buscar:

- Projects;
- Plans;
- Tasks;
- Runs;
- Workers;
- Workflows;
- artifacts;
- eventos/logs opcionalmente.

IDs cortos deben ser resolubles mientras no sean ambiguos.

---

## 38. Timeline

Cada Task/Run presenta timeline derivado de Events:

```text
09:14 Task created
09:16 Run #1 started on macbook
09:42 Checkpoint #1
10:03 Handoff requested
10:04 Run #2 resumed on linux
10:37 Needs input
10:41 User responded
11:08 Evaluators passed
11:12 Run completed
11:15 Promoted
```

No almacenar una segunda timeline mutable: derivarla del event store.

---

## 39. Historial y analytics

Consultas útiles:

- success rate por workflow/model/harness/project;
- duración media;
- tokens/run;
- cost/run;
- evaluator scores;
- worker utilization;
- queue wait;
- handoff frequency;
- failure reasons.

No usar estas métricas para decisiones automáticas sensibles hasta tener suficiente muestra y policy explícita.

---

## 40. Estados y errores

Errores deben tener códigos estables, por ejemplo:

```text
CF_WORKER_OFFLINE
CF_NO_ELIGIBLE_WORKER
CF_WORKFLOW_CONFLICT
CF_HARNESS_UNAVAILABLE
CF_SECRET_UNAVAILABLE
CF_BUDGET_EXCEEDED
CF_RUN_NOT_ATTACHABLE
CF_CHECKPOINT_UNSUPPORTED
CF_PROVIDER_USAGE_UNAVAILABLE
CF_NOT_FOUND
CF_AMBIGUOUS_REF
CF_INVALID_TRANSITION
CF_DEPENDENCY_CYCLE
CF_STALE_LEASE
CF_RUN_NOT_RESPONDABLE
CF_POLICY_DENIED
CF_CONFIRMATION_REQUIRED
CF_CONTROLLER_UNREACHABLE
CF_UNAUTHENTICATED
CF_VERSION_INCOMPATIBLE
CF_WORKER_CAPACITY
CF_WORKER_BUSY
CF_REPO_CONFIG_INVALID
CF_IDEMPOTENCY_MISMATCH
CF_IN_PROGRESS
CF_CURSOR_EXPIRED
CF_CONFIRMATION_STALE
CF_NOT_IMPLEMENTED
```

CLI puede mapearlos a exit codes/documentación; TUI a acciones recuperables.

Exit codes del CLI:

| Code | Significado | Errores |
|------|-------------|---------|
| 0 | ok | — |
| 1 | error no clasificado | `CF_REPO_CONFIG_INVALID`, `CF_INTERNAL` y otros |
| 2 | uso incorrecto | flags/args, `CF_INVALID_REQUEST` |
| 3 | no encontrado / ambiguo | `CF_NOT_FOUND`, `CF_AMBIGUOUS_REF` |
| 4 | conflicto de estado | `CF_INVALID_TRANSITION`, `CF_WORKFLOW_CONFLICT`, `CF_STALE_LEASE`, `CF_IDEMPOTENCY_MISMATCH`, `CF_CONFIRMATION_STALE`, `CF_CURSOR_EXPIRED`, `CF_DEPENDENCY_CYCLE` |
| 5 | denegado por policy/budget | `CF_POLICY_DENIED`, `CF_BUDGET_EXCEEDED`, `CF_SECRET_UNAVAILABLE` |
| 6 | Controller inaccesible / auth | `CF_CONTROLLER_UNREACHABLE`, `CF_UNAUTHENTICATED` |
| 7 | requiere confirmación | `CF_CONFIRMATION_REQUIRED` |
| 8 | capacidad no disponible | `CF_NO_ELIGIBLE_WORKER`, `CF_HARNESS_UNAVAILABLE`, `CF_CHECKPOINT_UNSUPPORTED`, `CF_RUN_NOT_RESPONDABLE`, `CF_WORKER_CAPACITY`, `CF_WORKER_BUSY`, `CF_NOT_IMPLEMENTED`, `CF_WORKER_OFFLINE`, `CF_RUN_NOT_ATTACHABLE`, `CF_VERSION_INCOMPATIBLE`, `CF_PROVIDER_USAGE_UNAVAILABLE` |
| 9 | reintentable | `CF_IN_PROGRESS` |

Con `--json`, los errores se emiten como `{"error": {"code": "...", "message": "...", "details": {...}}}` en stdout.

---

## 41. Compatibilidad/versionado

- API versionada.
- Controller debe conocer versiones de Workers.
- Upgrade policy: Controller N soporta Worker N y al menos N-1 cuando sea razonable.
- Workflow snapshots tienen schema version propio para metadata, aunque los blobs nativos permanezcan opacos.
- DB migrations automáticas con backup previo recomendado.

---

## 42. Testing

### 42.1 Unit

- domain state machines;
- scheduler eligibility/scoring;
- workflow merge/ownership;
- budget enforcement;
- usage aggregation;
- reconciliation decisions.

### 42.2 Integration

- Controller + fake Worker;
- real tmux runtime;
- SSH transport;
- Git/worktrees;
- SQLite recovery;
- disconnect/reconnect journal replay.

### 42.3 Adapter contract tests

Cada harness adapter debe pasar suite común:

- detect;
- install dry-run/test fixture;
- snapshot/apply/diff;
- start/stop;
- usage unsupported/supported behavior;
- no secrets in snapshots.

### 42.4 End-to-end

Escenarios obligatorios:

1. Task corre en Worker A y termina.
2. Mac client se cierra y Run continúa.
3. Controller reinicia y Run continúa/reconcilia.
4. Worker pierde red y reenvía events al volver.
5. Move A→B preserva source/checkpoint.
6. Workflow adopt→apply reproduce configuración.
7. Drift local no se sobrescribe sin policy/confirmación.
8. Fork dos Runs y compare.
9. MCP crea Task y consulta resultado.
10. Budget bloquea exceso.
11. Lease reasignada: el Worker original vuelve con epoch obsoleto y su proceso queda huérfano, no duplicado.
12. Un secret usado por un Run no aparece en logs, output persistido, snapshots ni backups.
13. Completion policy: Task con fork no se completa hasta el promote.
14. Reinicio del daemon del Worker con Runs activos: los Runs siguen y el journal no reutiliza secuencias.

---

## 43. Roadmap de implementación

### Fase 0 — Skeleton

- repo Go;
- domain IDs/entities;
- SQLite migrations;
- Controller API;
- Client SDK;
- Cobra CLI;
- event model;
- identidad del Controller, TLS y tokens de cliente (§16.6).

**Done:** `cf status` habla con Controller persistente sobre TLS autenticado.

### Fase 1 — Workers + ejecución básica

- worker daemon;
- join tokens + mTLS de Worker;
- register/heartbeat;
- leases con epoch (§10.4);
- aceptación de lease con CAS y arranque sólo tras confirmación (§10.4);
- estado local durable del Worker, wrapper de proceso y reconciliación al arrancar el daemon (§6.4);
- reconciliación mínima del Controller al reiniciar antes de programar trabajo (§27.1), con replay de reportes pendientes de ACK;
- publicación durable del resultado (§22.6) y git relay mínimo (§22.4);
- stop durable con confirmación (§10.7);
- autorización de Workers (§16.8);
- resource advertisement y reservas transaccionales (§10.5);
- SSH/Tailscale assumptions;
- tmux Runtime;
- Git worktree;
- Run: `queued|preparing|running|completed|failed|cancelled|lost`; Task: filas de §11.7 que no dependen de checkpoint, evaluators ni promote. Las operaciones de fases posteriores (defer, move, fork, promote) se rechazan explícitamente con `CF_NOT_IMPLEMENTED`;
- scheduler simple;
- `cf do`, `cf ps`, `cf attach`, `cf worker list`.

**Done:** lanzar un comando/harness en cualquiera de dos Workers y observarlo desde otro cliente; reiniciar Controller o daemon del Worker durante el Run no pierde ni duplica el Run ni su resultado.

### Fase 2 — Claude/Codex + Workflows

- Claude adapter;
- Codex adapter;
- detect/install/start;
- native config snapshot;
- `workflow adopt/status/diff/apply`, apply seguro respecto a Runs activos (§8.12);
- ownership `managed` / `local` / `ignored` (sin `merged`);
- secret refs con backend local al Worker y autorización mínima (§16.7);
- validación de `.codefabric/project.yaml` (§3.1);
- `doctor`;
- needs-input **no soportado** en Fases 1–2: los adapters configuran el harness para no bloquear nunca en input (permisos pre-concedidos). Si aun así un adapter detecta que el harness espera input, el Worker lo detiene con el stop de §10.7, el Run termina `failed` con `end_reason: input_required` y, tras `RunStopped`, se sella y publica como cualquier otro. La Task sigue la retry policy configurada sin excepciones (con el default `on_failed: none` queda `failed`; con `ask`, `waiting(decision)`). En modo `interactive` el usuario puede responder por `cf attach` antes de que eso ocurra, porque el adapter sólo lo declara en `headless`.

Se posponen a Fase 9: worker overlays, ownership `merged`, `workflow sync` a Workers offline.

**Done:** adoptar la configuración existente de un portátil y materializarla selectivamente en otro Worker sin copiar secretos.

### Fase 3 — TUI

- Bubble Tea shell;
- Activity home;
- Tasks;
- Workers;
- Workflows;
- Runs;
- logs;
- command palette;
- actions equivalentes al CLI;
- `waiting_input`, Inbox y `cf task respond` (§11.6, §19.5).

**Done:** operación cotidiana posible sin salir del TUI.

### Fase 4 — Logs, recovery y reconciliation

- log/output storage completos;
- worker journal con degradación por etapas (§1.3);
- replay completo de logs/output tras desconexión;
- retention;
- GC inicial (incluido relay).

**Done:** desconexiones/reinicios no pierden Runs ni historial observable.

### Fase 5 — Checkpoint/handoff

- checkpoint interface;
- defer/resume;
- move;
- lineage;
- automatic checkpoints.

**Done:** Task iniciada en A continúa mediante nuevo Run en B con contexto/source preservados.

### Fase 6 — Models/usage

- provider model;
- provider adapters iniciales;
- provider status/usage cuando disponible;
- harness usage events;
- attribution Project/Worker/Workflow/Harness/Run;
- Models TUI.

**Done:** ver consumo global oficial y desglose interno sin confundir ambos.

### Fase 7 — Experiments

- fork;
- evaluators;
- compare;
- Git diff;
- artifacts;
- promote;
- analytics básicos.

**Done:** ejecutar la misma Task con dos combinaciones y seleccionar/promover resultado.

### Fase 8 — MCP + Plans

- MCP server;
- semantic tools;
- permissions;
- Plan/DAG;
- event-driven wait/subscription;
- master budgets.

**Done:** un modelo puede dividir trabajo, lanzar Tasks y reaccionar a resultados sin shell arbitraria.

### Fase 9 — Policies/budgets/scheduler avanzado

- policy engine completo (lo mínimo de secrets y aislamiento ya existe desde Fases 1–2);
- worker overlays, ownership `merged`, sync a Workers offline;
- límites de concurrencia por harness/cuenta;
- resource quotas;
- budgets;
- scheduling strategies;
- model-limit-aware scheduling;
- historical metrics.

### Fase 10 — Elastic workers e integrations

- WorkerProvisioner;
- distribución de binarios y `cf worker upgrade` (§9.6);
- backend central de secrets (§16.7);
- Environment providers distintos de `host` (v0.x sólo `host` + scripts);
- cloud providers/plugins;
- notifications externas;
- hooks/CI integrations;
- optional remote artifact storage.

### 43.1 Mapeo a releases

| Release | Fases | Uso real posible |
|---------|-------|------------------|
| v0.1 | 0 + 1 | lanzar comandos/harness en Workers remotos y observarlos |
| v0.2 | 2 | uso diario con Claude/Codex y workflows sincronizados |
| v0.3 | 3 + 4 | TUI y tolerancia a fallos: sustituto del flujo manual SSH+tmux |
| v0.4 | 5 + 6 | handoff entre máquinas y visibilidad de consumo |
| v0.5 | 7 + 8 | experimentos y master model vía MCP |
| v1.0 | 9 + 10 | criterios de §44 completos |

Fase 4 puede adelantarse a Fase 3 si los Runs largos sobre portátiles resultan frágiles en v0.2.

---

## 44. Criterios de aceptación del producto completo

CodeFabric se considera funcionalmente completo respecto a esta especificación cuando:

1. Un Controller permanente coordina múltiples Workers heterogéneos.
2. Workers pueden unirse, drenar, desconectarse y volver sin corrupción de estado.
3. Tasks sobreviven conceptualmente a la máquina donde empezaron.
4. Runs son reproducibles/auditables y conservan lineage.
5. Claude/Codex configs existentes pueden adoptarse y sincronizarse total/parcialmente.
6. Secrets permanecen separados y sujetos a policy.
7. `cf` CLI cubre todas las operaciones públicas.
8. El TUI cubre las mismas operaciones y ofrece Activity/Inbox/Logs/Models.
9. MCP expone operaciones semánticas con permisos y budgets.
10. Logs/eventos/output sobreviven desconexiones según retention.
11. Model usage distingue provider-reported y Fabric-attributed.
12. Usage puede desglosarse por Project, Worker, Workflow, Harness, Task, Run y tiempo cuando hay datos observables.
13. Runs pueden forkearse, evaluarse, compararse y promoverse.
14. Controller puede respaldarse/restaurarse en otro host.
15. GC controla crecimiento de worktrees/logs/checkpoints/artifacts.
16. Un reinicio del Controller no mata Runs activos.
17. `cf doctor` identifica de forma accionable por qué un Worker no puede ejecutar una Task.
18. El sistema sigue siendo usable sin UI web, Kubernetes ni servicios externos aparte de los requeridos por los modelos/harnesses.

---

## 45. Decisiones que deben permanecer configurables

No fijar prematuramente:

- HTTP vs gRPC para RPC interno futuro;
- tmux vs runtime PTY/systemd/container futuro;
- SQLite vs Postgres futuro;
- filesystem blobs vs S3 futuro;
- Tailscale vs otros transports futuros;
- sistema concreto de secrets;
- sistema concreto de Environment;
- proveedor cloud para Workers efímeros.

Definir interfaces estables y empezar con la implementación más simple. Las interfaces de este documento (`Runtime`, `Transport`, `SecretStore`, `EnvironmentProvider`…) son **límites internos** con una única implementación hasta que haga falta la segunda; no son frameworks de plugins ni se publican como API estable.

---

## 46. Principios de diseño finales

1. **Task-centric, no machine-centric.**
2. **Native-first configuration.**
3. **Local-first control plane.**
4. **One core, many interfaces.** CLI, TUI y MCP son clientes.
5. **Observable by default.** Cada Run deja timeline, logs, artifacts y metadata.
6. **Portable, not magical.** Handoff significa reconstrucción reproducible, no live migration.
7. **Explicit over surprising.** Importar drift, promover resultados, distribuir secrets y destruir compute requieren intención clara.
8. **Graceful degradation.** Si una API no expone usage o un harness no soporta checkpoint, mostrarlo como capability ausente.
9. **Boring infrastructure.** Go + SQLite + Git + Tailscale + tmux deben ser suficientes para el MVP.
10. **Do not build a mini-Kubernetes.** Añadir complejidad sólo cuando resuelva una necesidad demostrada de CodeFabric.

---

## 47. Resumen operativo ideal

```bash
# Instalar Controller permanente
cf controller init
cf controller install-service

# Adoptar entorno existente
cf workflow adopt
cf workflow commit

# Añadir workers
cf worker join

# Abrir consola
cf

# Delegar trabajo
cf do "implement rate limiting" --project backend

# Ver todo
cf ps

# Cambiar de máquina
cf attach <task>

# Mover trabajo
cf task move <task> proxmox

# Experimentar
cf run fork <run> --workflow experimental@2
cf run compare <run-a> <run-b>
cf run promote <winner>

# Entender consumo
cf models usage --project backend

# Diagnóstico
cf logs --run <run> --follow
cf worker doctor <worker>

# Sincronizar entorno agentic
cf workflow diff --worker linux
cf workflow sync

# Mantener sistema
cf backup
cf gc --dry-run
```

La experiencia final debe sentirse como un único ordenador lógico para coding agents: el usuario piensa en **Projects → Tasks → Results**; CodeFabric se ocupa de **Workers → Workflows → Harnesses → Models → Logs → Scheduling**.

---

## Apéndice A — Defaults

| Parámetro | Default | Sección |
|-----------|---------|---------|
| `worker.heartbeat_interval` | 10 s | §5.3 |
| `worker.offline_ttl` | 45 s | §5.3 |
| `run.lost_timeout` | 30 min | §27.2 |
| `lease.accept_by` | 60 s | §10.4 |
| `run.resources` | cpu 2, memory 4096 MB | §10.5 |
| `worker.max_concurrent_runs` | 4 | §10.5 |
| `journal.max_size` | 1 GB | §1.3 |
| `journal.hard_max` | 5 GB | §1.3 |
| `journal.fsync` | 1 s o 64 entradas | §1.3 |
| `task.mode` | `headless` | §7.4 |
| `task.completion` | `first_success` | §11.7 |
| `task.retry` | `on_failed: none`, `on_lost: ask`, `max_attempts: 1` | §11.7 |
| `evaluator.timeout` | 15 min | §13 |
| `evaluator.required` | `true` | §13 |
| `fork.from` | `base` | §12.1 |
| `checkpoint.grace` | 30 s | §11.4 |
| `stop.grace` | 30 s | §10.7 |
| `checkpoint.exclude` | `.env*`, `*.pem`, `*.key` | §23.1 |
| `publication.max_attempts` | 10 (con Controller accesible) | §22.6 |
| `idempotency.ttl` | 24 h | §27.4 |
| `project.repo_config` | `allow` | §3.1 |
| `budget.warn_at` | 80 % | §15 |
| `budget.on_exceed` | `block_new` (`checkpoint_stop` para `max_duration`) | §15 |
| `budget.on_unobservable` | `warn` | §15 |
| `promote.strategy` | `branch`, con confirmación | §12.5 |
| `confirmation.ttl` | 24 h | §16.3 |
| `identity.cert_ttl` | 90 d | §16.6 |
| `events.wait.max_timeout` | 10 min | §17.3 |
| Retention | según §20.5 | §20.5 |

## Apéndice B — Cambios de la rev. 2

Completado:

- §1.3 plataformas, escala y objetivos no funcionales.
- §2 invariantes 16–18 (estado derivado de Task, leases, Runs terminales).
- §3.2 schema de Plan, reglas de DAG.
- §3.3/§3.4/§3.5 schemas de Task, Run y Worker con todos los campos que el resto de la spec usa (requirements, lineage, lease, effective config, environment, end/queued reason).
- §3.13 identificadores y resolución de referencias.
- §5.4 modelo de conectividad (Workers/clientes llaman al Controller; SSH sólo bootstrap/attach).
- §7.2 capacidades declaradas del harness, `Events`, `SendInput`; significado de `Host`.
- §7.4 modos `interactive` / `headless`.
- §8.11 referencias de Workflow y concurrencia optimista de commits.
- §9.5–9.6 máquina de estados del Worker, `undrain`, `remove`, upgrade del binario.
- §10.4–10.6 semántica de lease con fencing, reservas de recursos, explicabilidad del scheduler.
- §11.7 máquinas de estado de Run/Task, completion policy, retry policy; casos límite de move y respond.
- §12/§13/§15 punto de partida de fork, default de promote, ejecución de evaluators, reglas de budgets.
- §16.6–16.7 identidad/autenticación y `SecretStore`.
- §17 tools `events.wait`, `artifacts.*`, `budgets.*`, `confirmations.get`; flujo de confirmación.
- §20 eventos que otras secciones referenciaban y no estaban; deduplicación por Worker+epoch+seq.
- §22.4–22.5 git relay y layout en Worker; §23 schema de checkpoint y checkpoint mínimo; §25.3 definición de Environment.
- §40 códigos de error adicionales y exit codes; §43.1 mapeo a releases; §42.4 escenarios e2e 11–14.

Inconsistencias corregidas:

- Run: `checkpointing` faltaba en §3.4; añadido `suspended` para defer/move (§11.4 decía "detener o marcar suspendido según implementación").
- Task: `waiting/deferred` no existía como estado → `waiting` + `waiting_reason`; añadido `blocked` para dependencias.
- Worker: §9.3 usaba `drained`, ausente en §3.5.
- §27.2 `unknown` era un estado implícito → atributo `worker_reachable`.
- MCP: permisos en singular (`run.start`) vs tools en plural (`runs.start`) → un único vocabulario.
- CLI: `cf task run` (§11.1) vs `cf run "<prompt>"` (§18.3, §47) colisionaba con `cf run fork|compare|promote` → `cf task new` + atajo `cf do`.
- `RunBlockedByBudget` (§15) y `TaskNeedsInput`/`InputProvided` faltaban en §20.1.
- Esqueleto del CLI (§18.2) no incluía comandos usados en otras secciones (`attach`, `gc`, `restore`, `connect`, `setup`, `search`).

## Apéndice B2 — Cambios de la rev. 3 (revisión de Codex)

Blockers resueltos:

- §10.4 aceptación de lease por CAS, arranque sólo tras confirmación durable; §6.4 estado local del Worker, intención antes de efecto, `Runtime.Start` idempotente y wrapper de proceso.
- §43 recuperación mínima, publicación y autorización adelantadas a Fase 1.
- §8.12 configuración materializada por Run (`ConfigIsolation`) y reglas para harnesses sin aislamiento.
- §16.8 modelo de confianza y alcance de cada operación de un Worker; §16.2 aislamiento normativo vía elegibilidad.
- §22.6 protocolo de publicación del resultado; campos `publication` / `evaluation` en Run.

Important / minor:

- §11.7 tabla exhaustiva de estados de Task; transiciones de Run completas con arbitraje CAS; completion congelada con el primer Run.
- §11.5 move como operación durable por etapas, cercado por estado terminal del origen.
- §1.3 degradación del journal por etapas, con `JournalTruncated` en vez de prometer conservación ilimitada.
- §10.5 ciclo de vida de reservas; §27.4 contrato de `request_id` y transacción única estado+evento.
- §3.1/§3.2/§3.3/§15 defaults en Project/Plan, punto único de congelación, campos de budget.
- §16.6 bootstrap TLS con fingerprint en la invitación y canje atómico.
- §11.4/§23.1 captura sólo con el agente detenido; reglas de inclusión/exclusión.
- §16.3 confirmaciones vinculadas a la operación, que se ejecutan al aprobar.
- §3.1 `.codefabric/project.yaml` restringido por policies del Controller.
- `RunFinished` → `RunCompleted`; `--from checkpoint:<cp>` unificado; `cf models`; Runs archivados = Task archivada; `CF_CURSOR_EXPIRED`.

Recortes de MVP: máquina de estados reducida en Fase 1; overlays, `merged` y sync offline a Fase 9; upgrade de binarios, secrets centrales y Environments no-host a Fase 10; interfaces como límites internos (§45).

## Apéndice B3 — Cambios de la rev. 4 (segunda revisión de Codex)

- §10.4 la asignación se persiste en el Worker antes de `accept`; punto 8 recupera leases aceptadas sin registro local (`assignment_lost`).
- §10.4 punto 5 y §16.8: reportes de finalización y repeticiones idempotentes permitidos tras terminalidad con el mismo epoch; sólo los cambios de estado de ejecución y los epochs obsoletos se rechazan.
- §10.7 (nuevo) stop durable con confirmación `RunStopped`, independiente de checkpoints; campo `runtime` en Run.
- §10.5 la reserva se libera sólo con el proceso detenido y sin evaluación en curso; el Worker cuenta igual.
- §8.12/§10.1 lock de configuración por `(worker, harness)` con `config_key`, que cuenta leases pendientes; `apply_pending`; mutex local por harness.
- §22.6 `publication: pending` en la misma transacción que el exit (no existe `completed` + `none`); fallo del sellado con `cf run publish`.
- §27.4 `request_id` en la misma transacción que la mutación y su respuesta; operaciones asíncronas con registro durable reanudado en §27.1.
- §3.3/§3.4 congelación lógica (`spec_digest`) separada de la materialización en el Worker (`effective_config_digest`).
- §27.1/§27.3 reconciliación incluye operaciones asíncronas, stops pendientes y leases aceptadas.
- §43 needs-input fuera de Fases 1–2 (`end_reason: input_required`); `waiting_input` y respond pasan a Fase 3.
- Pendientes de checkpoint (quiesce vs exit espontáneo, procesos hijos en la captura) quedan para Fase 5.

## Apéndice B4 — Cambios de la rev. 4.1 (tercera revisión de Codex)

- §10.4 punto 8: una asignación sin registro local pero con sesión viva se adopta y reconstruye; no se marca como perdida.
- §10.7: todo Run que arrancó termina con `RunStopped` confirmado, también tras un exit normal; el sellado nunca empieza antes.
- §10.5: liberación inmediata con `runtime: none`; con harness arrancado, la reserva y el lock se retienen hasta parada + sellado + evaluación.
- §43 Fase 2: `input_required` detiene el harness por §10.7 antes de sellar y respeta la retry policy configurada.
- §10.7/§10.5 (ronda 4): cancel durante `preparing` serializado con el arranque en el Worker; la reserva sólo se libera tras `RunStopped(never_started)` o parada real.

## Apéndice B5 — Decisiones tomadas durante la implementación

- §11.7 fila 1: si una Task tiene los marcadores `archived` y `cancelled`, deriva `archived` (archivar es el acto final; GC y retention dependen de ello).
- §11.7 fila 10: «algún Run `completed` sin satisfacer la policy». Un resultado completado pendiente de revisión precede a la retry policy de Runs posteriores fallidos: la Task queda en `waiting(review)` y decide un humano.
- §40: `CF_DEPENDENCY_CYCLE` → exit 4; `CF_WORKER_OFFLINE`, `CF_RUN_NOT_ATTACHABLE`, `CF_VERSION_INCOMPATIBLE`, `CF_PROVIDER_USAGE_UNAVAILABLE` → exit 8.
- §40: `CF_INVALID_REQUEST` (petición mal formada o fuera de rango, HTTP 400) → exit 2 y `CF_INTERNAL` (fallo inesperado del servidor, sin detalles) → exit 1; no están en la lista de ejemplo de §40 pero el cable los necesita.
- §9.5: un heartbeat, un doctor en verde sobre un Worker `online` o un doctor fallido sobre un Worker `degraded` son no-ops.
- §3.13: una ref vacía nunca resuelve (`CF_NOT_FOUND`).
- §16.6 / §20.1: las mutaciones de identidad (`token`, `join`, `renew`, `revoke`) se auditan, pero no emiten eventos de dominio (§20.1 no los define) ni usan `request_id`: sus respuestas llevan secretos o certificados y no se guardan para reproducirlas. La idempotencia de `renew` la da el periodo de gracia: el certificado sustituido puede renovar de nuevo durante 10 minutos (solo `POST /v1/identity/renew`; revocar la anula), de modo que una respuesta perdida se recupera repitiendo `cf client renew`.
- §16.6: el certificado de servidor se renueva en caliente: el Controller lo revisa cada 12 h y, con menos de 30 días de vigencia, emite y publica uno nuevo sin reiniciar; las conexiones abiertas conservan el anterior.
- §16.6: las decisiones de autorización y caducidad de las mutaciones de identidad (`token`, `join`, `renew`, `revoke`) se toman dentro de la transacción, con el reloj leído después de adquirir el escritor y revalidando al caller (identidad existente, no revocada, certificado en vigencia y con el serial actual; `renew` admite además el anterior dentro de la gracia); `Authenticate` es solo un filtro por petición.
- §3.3 / §11.1 / §12.3: `spec_digest` es el sha256 del JSON de una spec tipada y versionada (`schema: 2`) con los valores ya resueltos Task → Project → Appendix A: `prompt`, `source` (ref efectivo, commit, snapshot), `mode`, `workflow`, `environment`, `requirements` (harness, model, capabilities, resources, workers), `completion` y `retry`. El prompt es un input y cuenta para la reproducibilidad: dos Runs con prompts distintos no son reproducciones. No incluye el nombre, IDs, timestamps ni estado, y las listas que son conjuntos (`capabilities`, `avoid`, `require`) se ordenan; `prefer` conserva su orden. Hasta la Fase 2, crear un Run de una Task o Project con `workflow`/`environment` no vacíos devuelve `CF_NOT_IMPLEMENTED`.
- §3.3: la retry policy se congela en la Task con el primer Run, igual que `completion` (columnas `retry_override` y `retry`). El estado derivado y la vista usan la congelada si existe y, si no, la heredada (override de la Task, Project, defaults); solo `cf task set --retry-on-failed|--retry-on-lost|--retry-max-attempts` (`PATCH /v1/tasks/{ref}` con `retry`) la cambia, y cambia también la congelada.
- §11.7: una Task terminada no recibe más Runs. Crear un Run de una Task con marcador `completed_manually_at`, `cancelled_at` o `archived_at`, o cuyo estado derivado sea terminal (`completed`, `failed`, `cancelled`, `archived`), devuelve `CF_INVALID_TRANSITION`.
- §16.7 / §3.1: el Controller nunca almacena una credencial de git. `cf project create` quita el userinfo de la URL del repo antes de enviarla (todo el userinfo en http(s), donde el nombre puede ser un token; solo la contraseña en el resto, de modo que `git@` en ssh se conserva) y avisa por stderr si quitó algo; el servidor rechaza con `CF_INVALID_REQUEST`, sin repetir la URL, cualquier `repo` con contraseña en el userinfo. Git usa las credenciales del propio usuario.
- §27.4: repetir un `request_id` revalida al caller antes de devolver la respuesta guardada, dentro de la misma transacción (`WithTx` → `RequireCaller` → buscar clave → ejecutar y guardar): una identidad revocada recibe `CF_UNAUTHENTICATED`, no una respuesta antigua.
- §3.13: un nombre de Task derivado del prompt que ya existe entre las no archivadas recibe `-2`, `-3`…; un nombre indicado que ya existe devuelve `CF_INVALID_REQUEST` (§40 no tiene un código de conflicto de unicidad). Los nombres son slugs que no pueden leerse como ID ni short ID.
- §3.13: el sufijo que el servidor añade a un nombre derivado del prompt nunca puede leerse como un ID: cada candidato final se valida como cualquier nombre, y si `<base>-<n>` parece un short ID (`task-2`) se usa `<base>-n<n>` (`task-n2`).
- §11.1 / ADR 0002 D1: `gitctx.Normalize` distingue repositorios distintos. Las rutas locales y `file://` conservan `.git` (`/srv/repo` y `/srv/repo.git` son claves distintas); en las URLs de red el puerto se descarta solo si es el de por defecto del esquema (ssh 22, https 443, http 80, git 9418) y cualquier otro forma parte de la clave. Sin coincidencia, error y ninguna Task.
- §3.3 / ADR 0002 D1: la petición de crear una Task lleva `resolved_ref` y `source_commit`, el par que resolvió el cliente, aparte de `source_ref`, el override persistente de la Task (vacío si hereda y que sigue heredando para futuros Runs). El Run congela en `source` el `resolved_ref` del request con su commit, aunque el default del Project cambie entre la resolución y la creación; sin `resolved_ref` ni commit, `CF_INVALID_REQUEST`.
- §27.4: el hash de idempotencia de `POST /v1/tasks` cubre solo los inputs explícitos del usuario (prompt, project, name, flags y overrides), no `resolved_ref` ni `source_commit`. Con `--request-id`, si el cliente no puede resolver la ref con git envía la petición sin el par: el servidor devuelve la respuesta guardada si la clave existe con el mismo hash, y `CF_INVALID_REQUEST` («source commit required») si no existe; un input explícito distinto da `CF_IDEMPOTENCY_MISMATCH`. Repetir una llamada no depende del remoto ni de que la rama no haya avanzado.
- ADR 0002 D1: `git ls-remote` tiene un timeout propio de 30 s (`lsRemoteTimeout`), un deadline de infraestructura sobre un contexto que corre en tiempo real; un remoto que no responde termina en error, no en un bloqueo.
- §11.7: cancelar y completar a mano una Task cancelan también sus Runs vivos en la misma transacción; archivar exige que no queden Runs vivos. Cancelar una Task ya terminada (`completed`, `failed`, `archived`) es `CF_INVALID_TRANSITION`; repetir una acción ya aplicada no hace nada.
- §3.13 / §27.4: `CF_AMBIGUOUS_REF` y `CF_IDEMPOTENCY_MISMATCH` viajan como HTTP 409.
- §18.2: `cf task defer|resume|move` y `cf run fork|promote` existen desde la Fase 1 y devuelven `CF_NOT_IMPLEMENTED` (exit 8) hasta su milestone.

## Apéndice C — Decisiones

| # | Decisión | Estado |
|---|----------|--------|
| 1 | Atajo de creación `cf do`; `cf run` es el namespace del recurso (§11.1) | aceptada |
| 2 | Git relay en el Controller (§22.4), con alcance mínimo en Fase 1 | aceptada |
| 3 | Completion default `first_success`, congelada con el primer Run (§11.7) | aceptada |
| 4 | Modo por defecto: `project.defaults.mode`, si no `headless`; nunca según el origen (§7.4) | cambiada en rev. 3 |
| 5 | Secrets v0.x locales al Worker, el Controller sólo guarda metadata; backend central después (§16.7) | cambiada en rev. 3 |
| 6 | `run.lost_timeout` de 30 min sin contar la caída del Controller (§27.2) | aceptada |
| 7 | `.codefabric/project.yaml` restringido por policies del Controller (§3.1) | aceptada |

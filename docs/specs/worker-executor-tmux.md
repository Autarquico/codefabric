---
codename: codefabric
title: Ejecutor del Worker con tmux, wrapper y journal
type: task
status: shipped
priority: P0
size: L
issue: 11
prs: [35]
related_issues: [11]
sub_issues: []
depends_on: [worker-registry-heartbeat]
supersedes: []
related_specs: [codefabric-spec, phase-1-workers]
created: 2026-10-10
updated: 2026-10-12
---

# Ejecutor del Worker con tmux, wrapper y journal

## Contexto / problema

Milestone de la [Fase 1 — Workers y ejecución básica](phase-1-workers.md). Implementa §6.1–6.4, §7.4, §10.4, §10.7, §20.4, §22.5 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng (devops-eng para tmux en CI).

## Scope

**Entra:**
- `internal/worker/state`: `state.db` (en `<data>/worker/`) con fases locales del Run, `journal` (la `pending_reports` de §6.4 y el journal de §20.4 son una tabla), `stop_orders`
- `internal/adapters/runtime/tmux`: socket aislado, sesión `cf-<run>-e<epoch>`, `Start` idempotente que adopta sesiones existentes
- `internal/worker/executor`: persistir asignación → accept → arrancar solo tras confirmación; lock por Run; stop
- `cf worker exec`: el wrapper lidera el grupo del panel (sin `Setpgid` ni PTY propio); pipes en `headless`, terminal del panel con `tmux pipe-pane` en `interactive`; `wrapper.json` (antes del spawn), `started.json`, `exit.json` y `output.log`; mata los restos tras `stop.grace`
- Identidad de proceso (pid + inicio) verificada antes de señalar: sin verificación no hay kill ni `RunStopped` (fase local `stop_unverified`)
- `internal/worker/journal`: append-only en `state.db`, escrito en la transacción del cambio que reporta (fsync por transacción, `synchronous=FULL`), envío en lotes de hasta 64 entradas y 48 KiB, con ACK y su epoch y seq originales
- Controller: `POST /v1/worker/runs/{run}/report` (`started`, `exited`, `prepare_failed`, `stopped`), evento `RunStopped`, migración `0104`, `max_concurrent_runs` y `allocatable` en la respuesta del heartbeat
- Orden desconocida: fail-closed (el cursor y el ACK no avanzan; el daemon informa `degraded`)
- `internal/adapters/harness/command`: harness genérico. Solo se ejecuta si el Run pide `harness: command`; el prompt es un script de shell POSIX que se ejecuta con `/bin/sh` desde `runs/<run-id>/command.sh`. Un Run sin harness recibe `CF_HARNESS_UNAVAILABLE`

**No entra:**
- Degradación del journal por etapas (Fase 4)
- La orden `stop` del Controller y `cancel → stop` (#13); aquí entran `Executor.Stop`, `stop_orders` y la ingesta de `RunStopped`
- Sellado y `result_commit` (#12): hasta entonces `RunStopped` libera la reserva aunque el harness haya arrancado (excepción temporal documentada en B5)

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Crash antes del accept
- **Given** una asignación persistida sin accept
- **When** el daemon reinicia
- **Then** reintenta el accept; con `CF_STALE_LEASE` descarta sin arrancar nada

### Scenario: Adopción de sesión
- **Given** `starting` persistido y la sesión tmux ya existente
- **When** el daemon reinicia
- **Then** adopta la sesión y solo hay una en `tmux ls`

### Scenario: Daemon muerto
- **Given** un Run corriendo
- **When** se hace `kill -9` al daemon y vuelve
- **Then** el proceso siguió vivo y su exit se lee de `exit.json` y se reporta una sola vez

### Scenario: Stop antes de arrancar
- **Given** un Run aceptado sin `starting`
- **When** llega un stop
- **Then** se persiste `aborted`, `Start` se niega y se reporta `RunStopped(never_started)`

### Scenario: Stop con hijos rebeldes
- **Given** un harness que ignora SIGTERM y tiene hijos
- **When** se ejecuta stop
- **Then** tras `stop.grace` no queda ningún proceso del grupo

### Scenario: Reenvío del journal
- **Given** 100 eventos sin ACK
- **When** el daemon reinicia
- **Then** se reenvían con su epoch y seq originales y el nuevo epoch no colisiona

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001, con depguard para `internal/worker/**` y `internal/adapters/**`) y las decisiones D1–D8 (ADR 0002).
- Las decisiones de este milestone están en el Apéndice B5 de `docs/codefabric-spec.md`.
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §6.1–6.4, §7.4, §10.4, §10.7, §20.4, §22.5

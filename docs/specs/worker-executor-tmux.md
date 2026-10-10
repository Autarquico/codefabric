---
codename: codefabric
title: Ejecutor del Worker con tmux, wrapper y journal
type: task
status: active
priority: P0
size: L
issue: 11
prs: []
related_issues: [11]
sub_issues: []
depends_on: [worker-registry-heartbeat]
supersedes: []
related_specs: [codefabric-spec, phase-1-workers]
created: 2026-10-10
updated: 2026-10-10
---

# Ejecutor del Worker con tmux, wrapper y journal

## Contexto / problema

Milestone de la [Fase 1 — Workers y ejecución básica](phase-1-workers.md). Implementa §6.1–6.4, §7.4, §10.4, §10.7, §20.4, §22.5 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng (devops-eng para tmux en CI).

## Scope

**Entra:**
- `internal/worker/state`: `state.db` con fases locales del Run, `pending_reports`, `stop_orders`
- `internal/adapters/runtime/tmux`: socket aislado, sesión `cf-<run>-e<epoch>`, `Start` idempotente que adopta sesiones existentes
- `internal/worker/executor`: persistir asignación → accept → arrancar solo tras confirmación; lock por Run; stop
- `cf worker exec`: `Setpgid`, PTY o pipes, `started.json`/`exit.json`/`output.log` atómicos; mata los restos tras `stop.grace`
- `internal/worker/journal`: append-only, fsync cada 1 s o 64 entradas, envío con ACK
- `internal/adapters/harness/command`: harness genérico que ejecuta un argv

**No entra:**
- Degradación del journal por etapas (Fase 4)

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

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §6.1–6.4, §7.4, §10.4, §10.7, §20.4, §22.5

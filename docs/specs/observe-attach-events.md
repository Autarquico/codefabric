---
codename: codefabric
title: "Observación: attach, ps, eventos y output"
type: task
status: active
priority: P0
size: M
issue: 14
prs: [38]
related_issues: [14]
sub_issues: []
depends_on: [worker-executor-tmux]
supersedes: []
related_specs: [codefabric-spec, phase-1-workers]
created: 2026-10-10
updated: 2026-10-13
---

# Observación: attach, ps, eventos y output

## Contexto / problema

Milestone de la [Fase 1 — Workers y ejecución básica](phase-1-workers.md). Implementa §5.4, §6.3, §20.2–20.3, §31.4 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: frontend-eng y backend-eng.

## Scope

**Entra:**
- `internal/app/events`; SSE `GET /v1/events/stream` y `GET /v1/events`
- `GET /v1/runs/{ref}/output` con tail y follow (cola en blobs; el almacenamiento completo llega en Fase 4)
- `internal/adapters/transport/ssh`; el Worker anuncia `ssh_target` en el heartbeat
- CLI: `cf attach`, `cf ps`, `cf events [--follow]`, `cf logs --run`

**No entra:**
- Logs estructurados y retention (Fase 4)
- Attach sin SSH (Fase 10)

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Attach
- **Given** un Run interactive en el Worker B
- **When** se ejecuta `cf attach auth-race` desde A
- **Then** se abre la sesión remota; cerrar el attach no termina el Run

### Scenario: Headless
- **Given** un Run headless
- **When** se ejecuta `cf attach`
- **Then** `CF_RUN_NOT_ATTACHABLE` y exit 8

### Scenario: Latencia
- **Given** un evento emitido en un Worker
- **When** se sigue `cf events --follow`
- **Then** aparece en menos de 2 s p95

### Scenario: Cliente cerrado (e2e #2)
- **Given** un Run en curso
- **When** se cierra el cliente Mac
- **Then** el Run continúa

### Scenario: ps
- **Given** Runs activos
- **When** se ejecuta `cf ps --json`
- **Then** incluye `worker_reachable` y `runtime`

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Decisiones de implementación

Las decisiones de este milestone (cursor y techo en un snapshot, ciclo de vida de SSE, output en lotes con anillo de SQLite, attach con override de confianza, matriz de autorización y riesgos residuales) están en el Apéndice B5 de `docs/codefabric-spec.md`, bajo el marcador `#14`.

## Referencias

- `docs/codefabric-spec.md` §5.4, §6.3, §20.2–20.3, §31.4

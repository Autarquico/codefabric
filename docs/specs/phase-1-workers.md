---
codename: codefabric
title: Fase 1 — Workers y ejecución básica
type: epic
status: shipped
priority: P0
size: XL
issue: 7
prs: []
related_issues: [7]
sub_issues: [8, 9, 10, 11, 12, 13, 14, 15, 16]
depends_on: [phase-0-skeleton]
supersedes: []
related_specs: [codefabric-spec]
created: 2026-10-10
updated: 2026-10-16
---

# Fase 1 — Workers y ejecución básica

## Contexto / problema

Fase 1 del roadmap de `docs/codefabric-spec.md` (§43). Release objetivo: **v0.1**.

**Done de la fase:** Lanzar un comando/harness en cualquiera de dos Workers y observarlo desde otro cliente; reiniciar Controller o daemon durante el Run no pierde ni duplica el Run ni su resultado.

## Scope

**Entra:** los milestones de la tabla. Cada uno tiene su propia spec con scenarios.

| Spec | Título | Size | Depende de |
|------|--------|------|------------|
| `projects-tasks-runs-api` | Projects, Tasks y Runs: API y CLI | M | `api-sdk-cli-status` |
| `worker-registry-heartbeat` | Registro de Workers y heartbeats | M | `api-sdk-cli-status` |
| `scheduler-leases-reservations` | Scheduler, leases y reservas | L | `projects-tasks-runs-api`, `worker-registry-heartbeat` |
| `worker-executor-tmux` | Ejecutor del Worker con tmux, wrapper y journal | L | `worker-registry-heartbeat` |
| `git-relay-publication` | Git relay y publicación del resultado | L | `projects-tasks-runs-api`, `worker-executor-tmux` |
| `stop-cancel-lost-reconcile` | Stop, cancel, lost y reconciliación | L | `scheduler-leases-reservations`, `worker-executor-tmux`, `git-relay-publication` |
| `observe-attach-events` | Observación: attach, ps, eventos y output | M | `worker-executor-tmux` |
| `service-units` | Unidades de servicio systemd y launchd | S | `worker-registry-heartbeat` |
| `e2e-v0-1` | E2E v0.1 | M | `stop-cancel-lost-reconcile`, `observe-attach-events`, `service-units` |

**No entra:** lo asignado a otras fases en §43.

## Criterios de aceptación

### Scenario: Done de la fase
- **Given** todos los milestones de la fase mergeados
- **When** se ejecuta su milestone `e2e-*` (si existe) y la verificación manual de la release
- **Then** se cumple: Lanzar un comando/harness en cualquiera de dos Workers y observarlo desde otro cliente; reiniciar Controller o daemon durante el Run no pierde ni duplica el Run ni su resultado.

## Referencias

- `docs/codefabric-spec.md` §43, §43.1
- `docs/adr/0002-implementation-decisions.md`

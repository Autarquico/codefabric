---
codename: codefabric
title: Scheduler, leases y reservas
type: task
status: shipped
priority: P0
size: L
issue: 10
prs: [34]
related_issues: [10]
sub_issues: []
depends_on: [projects-tasks-runs-api, worker-registry-heartbeat]
supersedes: []
related_specs: [codefabric-spec, phase-1-workers]
created: 2026-10-10
updated: 2026-10-11
---

# Scheduler, leases y reservas

## Contexto / problema

Milestone de la [Fase 1 — Workers y ejecución básica](phase-1-workers.md). Implementa §10.1, §10.3, §10.4, §10.5, §10.6, §31.3 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- `internal/app/scheduler`: filtros puros de elegibilidad con razones, score, bucle con una transacción por asignación, `explain`
- `internal/app/leases`: crear (con reserva y orden `assign` en la misma transacción), accept con CAS, reject, sweeper de `accept_by`
- Migración `0103_leases.sql`: `leases`, `reservations`, `worker_commands`
- Endpoints: `GET /v1/worker/assignments` (long-poll), accept/reject de leases, ACK de órdenes, `GET /v1/runs/{ref}/explain`
- CLI: `cf run explain`

**No entra:**
- Locks de configuración de harness (workflow-apply-drift-lock)
- Estrategias de scheduling (Fase 9)

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Determinismo
- **Given** dos Workers empatados
- **When** el scheduler decide 1000 veces
- **Then** elige siempre el mismo, por nombre

### Scenario: Expiración
- **Given** una lease no aceptada en 60 s
- **When** pasa el sweeper
- **Then** expira, libera la reserva y el Run se reasigna con `epoch+1`

### Scenario: Accept idempotente
- **Given** una lease aceptada / expirada
- **When** se repite el accept
- **Then** misma respuesta / `CF_STALE_LEASE`

### Scenario: Sin capacidad
- **Given** un Worker que rechaza con `CF_WORKER_CAPACITY`
- **When** se procesa el rechazo
- **Then** la reserva se libera y el Run sigue `queued`

### Scenario: Sin Worker elegible
- **Given** ningún Worker cumple los filtros
- **When** se consulta `cf run explain`
- **Then** el Run tiene `queued_reason: CF_NO_ELIGIBLE_WORKER` y se ve el filtro fallido de cada Worker

### Scenario: Drain
- **Given** un Worker `draining`
- **When** hay Runs en cola
- **Then** no recibe leases y sus reservas actuales se mantienen

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Decisiones de implementación

Detalladas en el Apéndice B5 de `docs/codefabric-spec.md`:

- La migración `0103_leases.sql` añade `leases`, `reservations` y `worker_commands` (solo el tipo `assign`), `workers.last_command_seq`, `workers.acked_seq` y `runs_queued`.
- `accept_by` es exclusivo; no hay estado `revoked` (una lease invalidada antes del accept queda `expired` con `close_code: run_ended`).
- `queued_reason` es siempre `CF_NO_ELIGIBLE_WORKER`; `idle` no es elegible; tras un rechazo o una expiración sin respuesta hay un backoff de 10 s (`domain.LeaseMissBackoff`).
- Accept y reject comparten un helper: Worker por identidad, lease propia, epoch, y solo entonces el estado.
- Long-poll con cursor `after_seq` persistido, reentrega hasta el ACK, `limit` y tope de bytes; un solo plazo de 20 s con el reloj inyectable.
- Cancelar o completar una Task expira la lease pendiente de su Run con su reserva; con la lease aceptada las conserva hasta la milestone del Worker.
- `cf run explain` y `GET /v1/runs/{ref}/explain`; `cf worker remove` también se bloquea con reservas abiertas.

## Referencias

- `docs/codefabric-spec.md` §10.1, §10.3, §10.4, §10.5, §10.6, §31.3

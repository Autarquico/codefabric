---
codename: codefabric
title: Stop, cancel, lost y reconciliación
type: task
status: active
priority: P0
size: L
issue: 13
prs: [37]
related_issues: [13]
sub_issues: []
depends_on:
  - scheduler-leases-reservations
  - worker-executor-tmux
  - git-relay-publication
supersedes: []
related_specs: [codefabric-spec, phase-1-workers]
created: 2026-10-10
updated: 2026-10-13
---

# Stop, cancel, lost y reconciliación

## Contexto / problema

Milestone de la [Fase 1 — Workers y ejecución básica](phase-1-workers.md). Implementa §10.4 (5, 8), §10.5, §10.7, §11.7, §27.1–27.3, invariantes 3, 11, 12, 17, 18 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng (qa-tester diseña la batería de fallos).

## Scope

**Entra:**
- `internal/app/runs/stop.go`: orden `stop` durable y campo `runtime`
- `internal/app/reconciliation`: arranque del Controller (scheduler bloqueado hasta terminar), reconexión de Worker, `lost` (deadline = max(último heartbeat, arranque del Controller) + timeout), huérfanos
- `internal/app/tasks/retry.go`
- `POST /v1/worker/reconcile`: entrada epoch, handles, finalizaciones pendientes, último seq; salida leases aceptadas, stops pendientes, ACK

**No entra:**
- Checkpoints antes de parar huérfanos (Fase 5)

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Cancel
- **Given** un Run `running`
- **When** se ejecuta `cf task cancel`
- **Then** `cancelled` inmediato, `runtime: active` hasta `RunStopped`, y la reserva solo se libera tras `RunStopped` y el sellado

### Scenario: Reinicio del Controller (e2e #3)
- **Given** Runs vivos
- **When** se reinicia el Controller
- **Then** los Runs siguen y convergen sin duplicados

### Scenario: Lost y huérfano
- **Given** un Worker desconectado 31 min con el Controller vivo
- **When** pasa el timeout
- **Then** el Run queda `lost` y la Task `waiting(decision)`; si el Worker vuelve con el proceso vivo, se ordena stop y se emite `RunOrphaned`

### Scenario: Caída del Controller no cuenta
- **Given** el Controller caído 40 min
- **When** arranca
- **Then** el temporizador de `lost` cuenta desde el arranque

### Scenario: Asignación sin registro local
- **Given** una lease aceptada sin registro en el Worker
- **When** el Worker reconecta
- **Then** sin sesión: `failed(assignment_lost)` + retry; con sesión viva: se reconstruye el registro y se adopta

### Scenario: Epochs
- **Given** un reporte con epoch obsoleto / de finalización con el epoch vigente sobre un Run terminal
- **When** llega al Controller
- **Then** `CF_STALE_LEASE` y stop del huérfano / se acepta

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Decisiones de implementación

Las decisiones de #13 (orden `stop` única por Worker, Run y epoch; reportes tardíos anotados sobre Runs que terminó el Controller; `harness_ran` como evidencia de liberación; reconciliación por rondas con fragmentos y páginas; puerta del scheduler; `lost` revalidado; reintento en `Rederive`; huérfanos conservados en el relay) están en el Apéndice B5 de `docs/codefabric-spec.md`.

Tests por scenario:

| Scenario | Tests |
|---|---|
| Cancel | `TestScenario_StopCancelLostReconcile_Cancel` (app), `_Cancel_Executor`, `_Cancel_RealStop` (executor, tmux), `_Cancel_E2E` (cmd/cf) |
| Reinicio del Controller | `_Arranque`, `_Arranque_TheGateOpensAtTheTTL` (app), `_ReinicioDelController` (cmd/cf, kill -9 del Controller) |
| Lost y huérfano | `_LostYHuerfano_Lost`, `_LostYHuerfano` (app), `_LostYHuerfano_E2E` (cmd/cf, kill -9 del daemon) |
| Caída del Controller no cuenta | `_CaidaDelControllerNoCuenta` (app), `_CaidaDelControllerNoCuenta_E2E` (cmd/cf) |
| Asignación sin registro local | `_AsignacionSinRegistroLocal` (app), `_AsignacionSinRegistroLocal_Executor`, `_AsignacionSinRegistroLocal_E2E` (cmd/cf) |
| Epochs | `_Epochs` (app), `_Epochs_Executor` |

## Referencias

- `docs/codefabric-spec.md` §10.4 (5, 8), §10.5, §10.7, §11.7, §27.1–27.3, invariantes 3, 11, 12, 17, 18

---
codename: codefabric
title: "Workflows: apply, drift y lock de configuración"
type: task
status: active
priority: P0
size: L
issue: 22
prs: [66, 68, 70]
related_issues: [22]
sub_issues: []
depends_on:
  - workflow-store-adopt-commit
  - claude-code-adapter
  - scheduler-leases-reservations
supersedes: []
related_specs: [codefabric-spec, phase-2-harnesses-workflows]
created: 2026-10-10
updated: 2026-11-01
---

# Workflows: apply, drift y lock de configuración

## Contexto / problema

Milestone de la [Fase 2 — Claude/Codex y Workflows](phase-2-harnesses-workflows.md). Implementa §8.3, §8.4, §8.6, §8.8, §8.12, §10.1, §27.4, invariante 14 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- `internal/app/workflows`: apply como operación durable, drift, explain, `configlock`
- Migración `0203_workflow_state.sql`: `worker_workflow_state`, `config_locks(worker, harness, config_key, holders, apply_pending)`
  - Nota: el número `0203` está ocupado (goose rechaza migraciones fuera de orden); la migración de estado toma el siguiente libre (>= 0207, la 0206 es `runs.effective_config_digest`).
- Worker: mutex por harness; `ApplyConfig` respeta ownership (`managed` se sobrescribe, `local` nunca)
- CLI: `cf workflow status|diff|apply|rollback|explain|sync` (sync solo a Workers online)

**Entrega en tres PRs:** 22-A (#66: el Worker usa el Workflow congelado del Run), 22-B1 (apply global, drift, rollback, dry-run, no-op, explain, sync y CLI; scenarios Drift local, Dry-run, No-op y Explain) y 22-B2 (lock §8.12, `config_key`, apply con Runs activos y `--no-wait`; scenarios Apply con Runs activos y Lock).

**No entra:**
- Overlays, `merged`, sync offline (Fase 9)

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Drift local (e2e #7)
- **Given** un componente `local` modificado
- **When** se ejecuta apply
- **Then** no se sobrescribe y `status` muestra drift

### Scenario: Apply con Runs activos
- **Given** un harness sin aislamiento con 2 Runs activos
- **When** se ejecuta apply
- **Then** `apply_pending`, sin leases nuevas de ese harness, y se aplica al llegar a 0; con `--no-wait`, `CF_WORKER_BUSY`

### Scenario: Lock de configuración
- **Given** un Run con `config_key` distinto del lock vigente
- **When** se evalúa la elegibilidad
- **Then** no es elegible en ese Worker y `explain` lo dice

### Scenario: Dry-run
- **Given** `--dry-run`
- **When** se ejecuta apply
- **Then** se muestra el diff y los digests no cambian

### Scenario: No-op
- **Given** el mismo desired digest
- **When** se repite el apply
- **Then** no hace nada

### Scenario: Explain
- **Given** un Worker y un harness
- **When** se ejecuta `explain`
- **Then** muestra el origen de cada componente antes de aplicar

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §8.3, §8.4, §8.6, §8.8, §8.12, §10.1, §27.4, invariante 14

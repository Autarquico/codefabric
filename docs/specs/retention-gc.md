---
codename: codefabric
title: Retention y GC
type: task
status: active
priority: P1
size: M
issue: 146
prs: [153]
related_issues: [146]
sub_issues: []
depends_on: [logs-output-storage, git-relay-publication]
supersedes: []
related_specs: [codefabric-spec, phase-4-recovery]
created: 2026-10-10
updated: 2026-11-08
---

# Retention y GC

## Contexto / problema

Milestone de la [Fase 4 — Logs, recovery y reconciliation](phase-4-recovery.md). Implementa §20.5, §29, §22.4, §22.6 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng y devops-eng.

## Scope

**Entra:**
- `internal/app/gc`: mark-and-sweep de blobs, retention en lotes; GC del relay; GC de worktrees en el Worker; `cf gc [--dry-run]`, `cf storage stats`

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Dry-run
- **Given** `cf gc --dry-run`
- **When** se ejecuta
- **Then** lista sin borrar

### Scenario: Publicación pendiente
- **Given** un worktree con `publication: pending|failed`
- **When** pasa el GC
- **Then** nunca se borra

### Scenario: Referencias vivas
- **Given** un artifact referenciado por un Run no archivado
- **When** pasa el GC
- **Then** se conserva

### Scenario: Latencia
- **Given** un GC grande
- **When** corre en paralelo con escrituras
- **Then** cada lote de borrado afecta como máximo a `gc.batch_size` filas en su propia transacción y cede entre lotes, de modo que ninguna escritura espera más de un lote. Es un invariante determinista en CI; el p95 real de escritura se mide en `make bench`

### Scenario: Storage stats
- **Given** blobs, logs y ramas en el relay del Controller
- **When** se ejecuta `cf storage stats`
- **Then** muestra los bytes por categoría y cuánto es recuperable por GC, sin modificar nada

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §20.5, §29, §22.4, §22.6

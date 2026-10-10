---
codename: codefabric
title: "Plans y DAG de dependencias"
type: task
status: draft
priority: P2
size: M
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [projects-tasks-runs-api]
supersedes: []
related_specs: [codefabric-spec, phase-8-mcp-plans]
created: 2026-10-10
updated: 2026-10-10
---

# Plans y DAG de dependencias

## Contexto / problema

Milestone de la [Fase 8 — MCP y Plans](phase-8-mcp-plans.md). Implementa §3.2, §10.3, §11.7 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- Tablas `plans`, `plan_tasks`, `task_dependencies`; DFS de ciclos en la transacción; estado del Plan derivado; `failure_policy`; `cf plan …`, `cf task new --depends-on`

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Ciclo
- **Given** dependencias que forman un ciclo
- **When** se crean
- **Then** `CF_DEPENDENCY_CYCLE`

### Scenario: Otro Project
- **Given** una dependencia de otro Project
- **When** se crea
- **Then** se rechaza

### Scenario: Draft
- **Given** un Plan en `draft`
- **When** pasa el scheduler
- **Then** no se programan Runs

### Scenario: failure_policy stop
- **Given** una Task fallida con `failure_policy: stop`
- **When** se procesa
- **Then** Plan `failed` y las pendientes no arrancan

### Scenario: Desbloqueo
- **Given** dependencias completadas
- **When** se recalcula
- **Then** la Task pasa de `blocked` a `queued`

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §3.2, §10.3, §11.7

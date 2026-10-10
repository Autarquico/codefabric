---
codename: codefabric
title: "Evaluators"
type: task
status: draft
priority: P2
size: L
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [artifacts, repo-config-environment-host]
supersedes: []
related_specs: [codefabric-spec, phase-7-experiments]
created: 2026-10-10
updated: 2026-10-10
---

# Evaluators

## Contexto / problema

Milestone de la [Fase 7 — Experiments](phase-7-experiments.md). Implementa §13, §10.5, §11.7 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- `internal/worker/evaluator`: worktree limpio en `result_commit`, timeout, parsers junit/go-test-json/TAP; reserva retenida; `cf run evaluate`

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Timeout
- **Given** un step que excede su timeout
- **When** se evalúa
- **Then** `status: error`, distinto de `failed`

### Scenario: Required fallido
- **Given** un step required fallido
- **When** termina la evaluación
- **Then** Task `waiting(review)`

### Scenario: Reserva
- **Given** una evaluación en curso
- **When** se consulta la reserva
- **Then** se retiene

### Scenario: Aislamiento
- **Given** el agente modifica el worktree vivo
- **When** se evalúa
- **Then** el evaluator usa `result_commit` y no lo ve

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §13, §10.5, §11.7

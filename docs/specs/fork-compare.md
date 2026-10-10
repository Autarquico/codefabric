---
codename: codefabric
title: "Fork y compare"
type: task
status: draft
priority: P2
size: M
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [checkpoint-capture, evaluators]
supersedes: []
related_specs: [codefabric-spec, phase-7-experiments]
created: 2026-10-10
updated: 2026-10-10
---

# Fork y compare

## Contexto / problema

Milestone de la [Fase 7 — Experiments](phase-7-experiments.md). Implementa §12.1–12.4 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng y frontend-eng.

## Scope

**Entra:**
- `POST /v1/runs/{ref}/fork {from, workflow, model, harness, worker}`; `GET /v1/runs/compare`; diff en el relay

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Base
- **Given** `--from base`
- **When** se hace fork
- **Then** mismo `source.commit`

### Scenario: Checkpoint
- **Given** `--from checkpoint:cp-3`
- **When** se hace fork
- **Then** parte del checkpoint

### Scenario: Compare (e2e #8)
- **Given** dos forks
- **When** se comparan
- **Then** estado, duración, diff y evaluaciones

### Scenario: Task completada
- **Given** una Task `completed`
- **When** se hace fork
- **Then** la Task sigue `completed`

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §12.1–12.4

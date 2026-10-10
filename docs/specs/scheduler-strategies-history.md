---
codename: codefabric
title: "Estrategias de scheduling e historial"
type: task
status: draft
priority: P2
size: M
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [analytics-history, concurrency-quotas-model-limits]
supersedes: []
related_specs: [codefabric-spec, phase-9-policies]
created: 2026-10-10
updated: 2026-10-10
---

# Estrategias de scheduling e historial

## Contexto / problema

Milestone de la [Fase 9 — Policies, budgets y scheduler avanzado](phase-9-policies.md). Implementa §10.2, §10.6, §39 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- Ver scenarios.

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Estrategia por Project
- **Given** una estrategia configurada
- **When** se ejecuta explain
- **Then** la muestra

### Scenario: Muestra insuficiente
- **Given** `historical-best` sin muestra
- **When** se decide
- **Then** cae al default

### Scenario: Determinismo
- **Given** la misma entrada
- **When** se decide con cualquier estrategia
- **Then** la misma decisión

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §10.2, §10.6, §39

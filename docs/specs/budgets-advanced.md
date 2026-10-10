---
codename: codefabric
title: "Budgets avanzados"
type: task
status: draft
priority: P2
size: M
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [budgets-core, defer-resume]
supersedes: []
related_specs: [codefabric-spec, phase-9-policies]
created: 2026-10-10
updated: 2026-10-10
---

# Budgets avanzados

## Contexto / problema

Milestone de la [Fase 9 — Policies, budgets y scheduler avanzado](phase-9-policies.md). Implementa §15 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- Ver scenarios.

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: checkpoint_stop
- **Given** `on_exceed: checkpoint_stop`
- **When** se supera el budget
- **Then** los Runs activos pasan a `suspended`

### Scenario: max_duration
- **Given** `max_duration` sin `on_exceed`
- **When** se supera
- **Then** usa `checkpoint_stop`

### Scenario: Unobservable block
- **Given** `on_unobservable: block`
- **When** se lanza un Run sin usage observable
- **Then** no se lanza

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §15

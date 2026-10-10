---
codename: codefabric
title: "Promote"
type: task
status: draft
priority: P2
size: M
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [fork-compare]
supersedes: []
related_specs: [codefabric-spec, phase-7-experiments]
created: 2026-10-10
updated: 2026-10-10
---

# Promote

## Contexto / problema

Milestone de la [Fase 7 — Experiments](phase-7-experiments.md). Implementa §12.5, §11.7 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- Orden `promote` a un Worker con el secret de origin (D3); estrategia `branch` por defecto, `merge` opt-in; `--replace`; confirmación; evaluators `before_promote`

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Completion promote (e2e #13)
- **Given** `completion: promote` con 2 forks
- **When** no se ha promovido nada
- **Then** la Task no se completa hasta el promote

### Scenario: Segundo promote
- **Given** un Run ya promovido
- **When** se promueve otro sin `--replace`
- **Then** se rechaza

### Scenario: Origin limpio
- **Given** Runs sin promover
- **When** se inspecciona origin
- **Then** no hay ramas `cf/*`

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §12.5, §11.7

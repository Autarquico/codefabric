---
codename: codefabric
title: "E2E del TUI"
type: task
status: draft
priority: P1
size: S
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [needs-input-inbox, palette-search-notify]
supersedes: []
related_specs: [codefabric-spec, phase-3-tui]
created: 2026-10-10
updated: 2026-10-10
---

# E2E del TUI

## Contexto / problema

Milestone de la [Fase 3 — TUI](phase-3-tui.md). Implementa Done de Fase 3 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: qa-tester.

## Scope

**Entra:**
- Ver scenarios.

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Done de Fase 3
- **Given** un guion de Update/View
- **When** se crea Task, se responde input, se cancela, se drena un Worker y se aplica un workflow
- **Then** todo sin salir del TUI

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` Done de Fase 3

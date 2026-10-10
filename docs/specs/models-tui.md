---
codename: codefabric
title: "Vista Models del TUI"
type: task
status: draft
priority: P1
size: M
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [usage-attribution, tui-views-actions]
supersedes: []
related_specs: [codefabric-spec, phase-6-models-usage]
created: 2026-10-10
updated: 2026-10-10
---

# Vista Models del TUI

## Contexto / problema

Milestone de la [Fase 6 — Models/usage](phase-6-models-usage.md). Implementa §19.3–19.4 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: frontend-eng.

## Scope

**Entra:**
- Ver scenarios.

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Porcentajes
- **Given** provider con / sin cuota reportada
- **When** se muestra
- **Then** porcentaje / usage etiquetado

### Scenario: Drill-down
- **Given** la vista Models
- **When** se navega
- **Then** de provider a Run

### Scenario: Etiquetas
- **Given** las dos fuentes
- **When** se muestran
- **Then** se distinguen provider-reported y Fabric-attributed

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §19.3–19.4

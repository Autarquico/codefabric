---
codename: codefabric
title: "Move y handoff"
type: task
status: draft
priority: P1
size: L
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [defer-resume]
supersedes: []
related_specs: [codefabric-spec, phase-5-handoff]
created: 2026-10-10
updated: 2026-10-10
---

# Move y handoff

## Contexto / problema

Milestone de la [Fase 5 — Checkpoint/handoff](phase-5-handoff.md). Implementa §11.5, §9.3, §23.2 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- Tabla `moves` con etapas durables reanudadas al arrancar (§27.1); `cf task move`, `cf worker drain --move`

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Move (e2e #5)
- **Given** un Run en A
- **When** se mueve a B
- **Then** el destino solo se crea con el origen terminal; source y checkpoint se preservan

### Scenario: Reinicio
- **Given** el Controller reinicia en `target_created`
- **When** arranca
- **Then** el move continúa

### Scenario: Origen offline
- **Given** el Worker origen offline
- **When** se pide el move
- **Then** se exige `lost` y se muestra lo que se perdería

### Scenario: Destino falla
- **Given** la preparación del destino falla
- **When** se procesa
- **Then** Task `waiting(decision)` y reintento desde la misma etapa

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §11.5, §9.3, §23.2

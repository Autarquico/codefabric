---
codename: codefabric
title: "Overlays, `merged` y sync offline"
type: task
status: draft
priority: P2
size: L
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [workflow-apply-drift-lock]
supersedes: []
related_specs: [codefabric-spec, phase-9-policies]
created: 2026-10-10
updated: 2026-10-10
---

# Overlays, `merged` y sync offline

## Contexto / problema

Milestone de la [Fase 9 — Policies, budgets y scheduler avanzado](phase-9-policies.md). Implementa §8.3, §8.4, §8.5, §8.7 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- Overlays por Worker; `merged` por adapter con estrategia declarada; sync a Workers offline al reconectar; escaneo de drift (`WorkflowDriftDetected`)

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Overlay
- **Given** un overlay que deshabilita el MCP `xcode` en linux
- **When** se ejecuta explain
- **Then** lo refleja

### Scenario: Merged
- **Given** un componente `merged`
- **When** se aplica
- **Then** se preservan las extensiones locales

### Scenario: Offline
- **Given** un Worker offline
- **When** reconecta
- **Then** se aplica la versión deseada

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §8.3, §8.4, §8.5, §8.7

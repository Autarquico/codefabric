---
codename: codefabric
title: "OpenRouter, Google y cuota vía harness"
type: task
status: draft
priority: P1
size: M
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [provider-anthropic-openai]
supersedes: []
related_specs: [codefabric-spec, phase-6-models-usage]
created: 2026-10-10
updated: 2026-10-10
---

# OpenRouter, Google y cuota vía harness

## Contexto / problema

Milestone de la [Fase 6 — Models/usage](phase-6-models-usage.md). Implementa §14.2–14.3 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- OpenRouter `/api/v1/key`; Google solo status; `rate_limits` de Codex como provider-reported con `source: harness`

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: OpenRouter
- **Given** una key de OpenRouter
- **When** se consulta
- **Then** límite y uso reportados

### Scenario: Cuota vía harness
- **Given** `rate_limits` de Codex
- **When** se muestra
- **Then** porcentaje con fuente «harness»

### Scenario: Google
- **Given** Google
- **When** se consulta usage
- **Then** `unsupported`

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §14.2–14.3

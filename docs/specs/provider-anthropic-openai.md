---
codename: codefabric
title: "Providers Anthropic y OpenAI"
type: task
status: draft
priority: P1
size: M
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [usage-attribution]
supersedes: []
related_specs: [codefabric-spec, phase-6-models-usage]
created: 2026-10-10
updated: 2026-10-10
---

# Providers Anthropic y OpenAI

## Contexto / problema

Milestone de la [Fase 6 — Models/usage](phase-6-models-usage.md). Implementa §14.2–14.3 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- Status por Statuspage; usage/coste por Admin API con admin key (D4); `provider_usage_snapshots`; `cf models [status]`

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Sin key
- **Given** sin admin key
- **When** se consulta usage
- **Then** `unsupported` y status disponible

### Scenario: Degradación
- **Given** una incidencia en Statuspage
- **When** se sondea
- **Then** `ProviderStatusChanged`

### Scenario: Separación
- **Given** cifras del provider y de Fabric
- **When** se muestran
- **Then** en columnas separadas (invariante 10)

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §14.2–14.3

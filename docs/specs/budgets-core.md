---
codename: codefabric
title: "Budgets (núcleo)"
type: task
status: draft
priority: P2
size: M
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [usage-attribution, plans-dag]
supersedes: []
related_specs: [codefabric-spec, phase-8-mcp-plans]
created: 2026-10-10
updated: 2026-10-10
---

# Budgets (núcleo)

## Contexto / problema

Milestone de la [Fase 8 — MCP y Plans](phase-8-mcp-plans.md). Implementa §15 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- Evaluador jerárquico Run→Task→Plan→Project; `max_runs`, `max_parallel`, `max_tokens`, `max_cost`; `budgets.get|raise`; `cf budget show|set|raise`

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Aviso
- **Given** consumo al 80 %
- **When** se evalúa
- **Then** `BudgetWarning`

### Scenario: Exceso (e2e #10)
- **Given** budget superado
- **When** se crea un Run
- **Then** queda `queued` con `RunBlockedByBudget`

### Scenario: Partial
- **Given** una suscripción sin usage
- **When** se evalúa
- **Then** budget `partial`

### Scenario: Raise vía MCP
- **Given** `budgets.raise` desde MCP
- **When** se llama
- **Then** `CF_CONFIRMATION_REQUIRED`

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §15

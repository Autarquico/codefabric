---
codename: codefabric
title: "Fase 9 — Policies, budgets y scheduler avanzado"
type: epic
status: draft
priority: P2
size: XL
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [phase-8-mcp-plans]
supersedes: []
related_specs: [codefabric-spec]
created: 2026-10-10
updated: 2026-10-10
---

# Fase 9 — Policies, budgets y scheduler avanzado

## Contexto / problema

Fase 9 del roadmap de `docs/codefabric-spec.md` (§43). Release objetivo: **v1.0**.

**Done de la fase:** Policies, budgets completos y estrategias de scheduling configurables.

## Scope

**Entra:** los milestones de la tabla. Cada uno tiene su propia spec con scenarios.

| Spec | Título | Size | Depende de |
|------|--------|------|------------|
| `policy-engine` | Policy engine | L | `secrets-local`, `confirmations-sessions-permissions` |
| `workflow-overlays-merged-offline` | Overlays, `merged` y sync offline | L | `workflow-apply-drift-lock` |
| `budgets-advanced` | Budgets avanzados | M | `budgets-core`, `defer-resume` |
| `concurrency-quotas-model-limits` | Concurrencia, quotas y límites de modelo | M | `policy-engine`, `provider-anthropic-openai` |
| `scheduler-strategies-history` | Estrategias de scheduling e historial | M | `analytics-history`, `concurrency-quotas-model-limits` |

**No entra:** lo asignado a otras fases en §43.

## Criterios de aceptación

### Scenario: Done de la fase
- **Given** todos los milestones de la fase mergeados
- **When** se ejecuta su milestone `e2e-*` (si existe) y la verificación manual de la release
- **Then** se cumple: Policies, budgets completos y estrategias de scheduling configurables.

## Referencias

- `docs/codefabric-spec.md` §43, §43.1
- `docs/adr/0002-implementation-decisions.md`

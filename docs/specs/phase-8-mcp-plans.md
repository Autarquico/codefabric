---
codename: codefabric
title: "Fase 8 — MCP y Plans"
type: epic
status: draft
priority: P2
size: XL
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [phase-7-experiments]
supersedes: []
related_specs: [codefabric-spec]
created: 2026-10-10
updated: 2026-10-10
---

# Fase 8 — MCP y Plans

## Contexto / problema

Fase 8 del roadmap de `docs/codefabric-spec.md` (§43). Release objetivo: **v0.5**.

**Done de la fase:** Un modelo puede dividir trabajo, lanzar Tasks y reaccionar a resultados sin shell arbitraria.

## Scope

**Entra:** los milestones de la tabla. Cada uno tiene su propia spec con scenarios.

| Spec | Título | Size | Depende de |
|------|--------|------|------------|
| `plans-dag` | Plans y DAG de dependencias | M | `projects-tasks-runs-api` |
| `confirmations-sessions-permissions` | Confirmaciones, sesiones MCP y permisos | M | `projects-tasks-runs-api` |
| `mcp-server` | Servidor MCP | L | `plans-dag`, `confirmations-sessions-permissions`, `client-subscriptions` |
| `budgets-core` | Budgets (núcleo) | M | `usage-attribution`, `plans-dag` |
| `e2e-v0-5` | E2E v0.5 | S | `mcp-server`, `budgets-core`, `promote` |

**No entra:** lo asignado a otras fases en §43.

## Criterios de aceptación

### Scenario: Done de la fase
- **Given** todos los milestones de la fase mergeados
- **When** se ejecuta su milestone `e2e-*` (si existe) y la verificación manual de la release
- **Then** se cumple: Un modelo puede dividir trabajo, lanzar Tasks y reaccionar a resultados sin shell arbitraria.

## Referencias

- `docs/codefabric-spec.md` §43, §43.1
- `docs/adr/0002-implementation-decisions.md`

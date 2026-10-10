---
codename: codefabric
title: "Fase 7 — Experiments"
type: epic
status: draft
priority: P2
size: XL
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [phase-6-models-usage]
supersedes: []
related_specs: [codefabric-spec]
created: 2026-10-10
updated: 2026-10-10
---

# Fase 7 — Experiments

## Contexto / problema

Fase 7 del roadmap de `docs/codefabric-spec.md` (§43). Release objetivo: **v0.5**.

**Done de la fase:** Ejecutar la misma Task con dos combinaciones y seleccionar/promover el resultado.

## Scope

**Entra:** los milestones de la tabla. Cada uno tiene su propia spec con scenarios.

| Spec | Título | Size | Depende de |
|------|--------|------|------------|
| `artifacts` | Artifacts | M | `git-relay-publication`, `logs-output-storage` |
| `evaluators` | Evaluators | L | `artifacts`, `repo-config-environment-host` |
| `fork-compare` | Fork y compare | M | `checkpoint-capture`, `evaluators` |
| `promote` | Promote | M | `fork-compare` |
| `analytics-history` | Analytics e historial | S | `evaluators`, `usage-attribution` |

**No entra:** lo asignado a otras fases en §43.

## Criterios de aceptación

### Scenario: Done de la fase
- **Given** todos los milestones de la fase mergeados
- **When** se ejecuta su milestone `e2e-*` (si existe) y la verificación manual de la release
- **Then** se cumple: Ejecutar la misma Task con dos combinaciones y seleccionar/promover el resultado.

## Referencias

- `docs/codefabric-spec.md` §43, §43.1
- `docs/adr/0002-implementation-decisions.md`

---
codename: codefabric
title: "Fase 5 — Checkpoint/handoff"
type: epic
status: draft
priority: P1
size: XL
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [phase-4-recovery]
supersedes: []
related_specs: [codefabric-spec]
created: 2026-10-10
updated: 2026-10-10
---

# Fase 5 — Checkpoint/handoff

## Contexto / problema

Fase 5 del roadmap de `docs/codefabric-spec.md` (§43). Release objetivo: **v0.4**.

**Done de la fase:** Una Task iniciada en A continúa mediante un Run nuevo en B con contexto y source preservados.

## Scope

**Entra:** los milestones de la tabla. Cada uno tiene su propia spec con scenarios.

| Spec | Título | Size | Depende de |
|------|--------|------|------------|
| `checkpoint-capture` | Captura de checkpoints | L | `claude-code-adapter`, `codex-adapter`, `logs-output-storage` |
| `defer-resume` | Defer y resume | M | `checkpoint-capture` |
| `move-handoff` | Move y handoff | L | `defer-resume` |
| `dirty-snapshots-auto-checkpoints` | Snapshots dirty y checkpoints automáticos | M | `checkpoint-capture` |

**No entra:** lo asignado a otras fases en §43.

## Criterios de aceptación

### Scenario: Done de la fase
- **Given** todos los milestones de la fase mergeados
- **When** se ejecuta su milestone `e2e-*` (si existe) y la verificación manual de la release
- **Then** se cumple: Una Task iniciada en A continúa mediante un Run nuevo en B con contexto y source preservados.

## Referencias

- `docs/codefabric-spec.md` §43, §43.1
- `docs/adr/0002-implementation-decisions.md`

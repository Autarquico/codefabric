---
codename: codefabric
title: Fase 2 — Claude/Codex y Workflows
type: epic
status: active
priority: P0
size: XL
issue: 17
prs: []
related_issues: [17]
sub_issues: [18, 19, 20, 21, 22, 23, 24, 25, 26]
depends_on: [phase-1-workers]
supersedes: []
related_specs: [codefabric-spec]
created: 2026-10-10
updated: 2026-10-10
---

# Fase 2 — Claude/Codex y Workflows

## Contexto / problema

Fase 2 del roadmap de `docs/codefabric-spec.md` (§43). Release objetivo: **v0.2**.

**Done de la fase:** Adoptar la configuración existente de un portátil y materializarla selectivamente en otro Worker sin copiar secretos.

## Scope

**Entra:** los milestones de la tabla. Cada uno tiene su propia spec con scenarios.

| Spec | Título | Size | Depende de |
|------|--------|------|------------|
| `harness-adapter-framework` | Framework de adapters de harness | M | `worker-executor-tmux`, `git-relay-publication` |
| `workflow-store-adopt-commit` | Workflows: almacenamiento, adopt y commit | L | `projects-tasks-runs-api`, `harness-adapter-framework` |
| `claude-code-adapter` | Adapter de Claude Code | L | `harness-adapter-framework` |
| `codex-adapter` | Adapter de Codex CLI | M | `harness-adapter-framework` |
| `workflow-apply-drift-lock` | Workflows: apply, drift y lock de configuración | L | `workflow-store-adopt-commit`, `claude-code-adapter`, `scheduler-leases-reservations` |
| `secrets-local` | Secrets locales al Worker | M | `scheduler-leases-reservations`, `worker-executor-tmux`, `harness-adapter-framework` |
| `repo-config-environment-host` | Config del repo y Environment host | M | `git-relay-publication`, `secrets-local` |
| `doctor-and-join-wizard` | Doctor y wizard de join | M | `claude-code-adapter`, `codex-adapter`, `workflow-apply-drift-lock`, `secrets-local`, `observe-attach-events` |
| `e2e-v0-2` | E2E v0.2 | S | `doctor-and-join-wizard`, `repo-config-environment-host` |

**No entra:** lo asignado a otras fases en §43.

## Criterios de aceptación

### Scenario: Done de la fase
- **Given** todos los milestones de la fase mergeados
- **When** se ejecuta su milestone `e2e-*` (si existe) y la verificación manual de la release
- **Then** se cumple: Adoptar la configuración existente de un portátil y materializarla selectivamente en otro Worker sin copiar secretos.

## Referencias

- `docs/codefabric-spec.md` §43, §43.1
- `docs/adr/0002-implementation-decisions.md`

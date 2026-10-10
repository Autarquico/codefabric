---
codename: codefabric
title: "Fase 10 — Elastic workers e integrations"
type: epic
status: draft
priority: P2
size: XL
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [phase-9-policies]
supersedes: []
related_specs: [codefabric-spec]
created: 2026-10-10
updated: 2026-10-10
---

# Fase 10 — Elastic workers e integrations

## Contexto / problema

Fase 10 del roadmap de `docs/codefabric-spec.md` (§43). Release objetivo: **v1.0**.

**Done de la fase:** Criterios de §44 completos.

## Scope

**Entra:** los milestones de la tabla. Cada uno tiene su propia spec con scenarios.

| Spec | Título | Size | Depende de |
|------|--------|------|------------|
| `binary-distribution-upgrade` | Distribución de binarios y upgrade | M | `service-units` |
| `worker-provisioner` | Worker provisioner | L | `binary-distribution-upgrade`, `budgets-core` |
| `central-secrets` | Backend central de secrets | M | `secrets-local` |
| `docker-environment-runtime` | Environment y Runtime docker | L | `policy-engine`, `repo-config-environment-host` |
| `env-providers-devcontainer-mise-nix` | Environment providers devcontainer, mise y nix | M | `docker-environment-runtime` |
| `notifications-integrations` | Notificaciones e integraciones | M | `palette-search-notify`, `promote` |
| `opencode-pi-adapters` | Adapters de OpenCode y Pi | M | `harness-adapter-framework` |
| `remote-artifact-storage` | Almacenamiento remoto de artifacts | S | `retention-gc` |
| `pty-relay-attach` | Attach por relay de PTY | M | `observe-attach-events` |
| `v1-acceptance` | Aceptación v1.0 | M | `worker-provisioner`, `central-secrets`, `env-providers-devcontainer-mise-nix`, `notifications-integrations`, `opencode-pi-adapters`, `remote-artifact-storage`, `pty-relay-attach`, `scheduler-strategies-history`, `budgets-advanced`, `workflow-overlays-merged-offline`, `e2e-v0-5`, `e2e-v0-3`, `e2e-v0-3-tui`, `move-handoff`, `dirty-snapshots-auto-checkpoints`, `models-tui`, `provider-openrouter-google-harness-quota`, `e2e-v0-2` |

**No entra:** lo asignado a otras fases en §43.

## Criterios de aceptación

### Scenario: Done de la fase
- **Given** todos los milestones de la fase mergeados
- **When** se ejecuta su milestone `e2e-*` (si existe) y la verificación manual de la release
- **Then** se cumple: Criterios de §44 completos.

## Referencias

- `docs/codefabric-spec.md` §43, §43.1
- `docs/adr/0002-implementation-decisions.md`

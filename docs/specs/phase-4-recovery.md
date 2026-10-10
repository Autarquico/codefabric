---
codename: codefabric
title: "Fase 4 — Logs, recovery y reconciliation"
type: epic
status: draft
priority: P1
size: XL
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [phase-3-tui]
supersedes: []
related_specs: [codefabric-spec]
created: 2026-10-10
updated: 2026-10-10
---

# Fase 4 — Logs, recovery y reconciliation

## Contexto / problema

Fase 4 del roadmap de `docs/codefabric-spec.md` (§43). Release objetivo: **v0.3**.

**Done de la fase:** Desconexiones y reinicios no pierden Runs ni historial observable.

## Scope

**Entra:** los milestones de la tabla. Cada uno tiene su propia spec con scenarios.

| Spec | Título | Size | Depende de |
|------|--------|------|------------|
| `logs-output-storage` | Almacenamiento de logs y output | M | `observe-attach-events` |
| `journal-degradation-replay` | Degradación del journal y replay | M | `logs-output-storage` |
| `retention-gc` | Retention y GC | M | `logs-output-storage`, `git-relay-publication` |
| `backup-restore` | Backup y restore | M | `retention-gc` |
| `e2e-v0-3` | E2E v0.3 (recovery) | S | `journal-degradation-replay`, `backup-restore` |

**No entra:** lo asignado a otras fases en §43.

## Criterios de aceptación

### Scenario: Done de la fase
- **Given** todos los milestones de la fase mergeados
- **When** se ejecuta su milestone `e2e-*` (si existe) y la verificación manual de la release
- **Then** se cumple: Desconexiones y reinicios no pierden Runs ni historial observable.

## Referencias

- `docs/codefabric-spec.md` §43, §43.1
- `docs/adr/0002-implementation-decisions.md`

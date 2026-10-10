---
codename: codefabric
title: "Fase 3 — TUI"
type: epic
status: draft
priority: P1
size: XL
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [phase-2-harnesses-workflows]
supersedes: []
related_specs: [codefabric-spec]
created: 2026-10-10
updated: 2026-10-10
---

# Fase 3 — TUI

## Contexto / problema

Fase 3 del roadmap de `docs/codefabric-spec.md` (§43). Release objetivo: **v0.3**.

**Done de la fase:** Operación cotidiana posible sin salir del TUI.

## Scope

**Entra:** los milestones de la tabla. Cada uno tiene su propia spec con scenarios.

| Spec | Título | Size | Depende de |
|------|--------|------|------------|
| `client-subscriptions` | Suscripciones del Client SDK | S | `observe-attach-events` |
| `tui-shell-home` | TUI: shell y home | M | `client-subscriptions` |
| `tui-views-actions` | TUI: vistas y acciones | L | `tui-shell-home` |
| `needs-input-inbox` | Needs-input e Inbox | L | `claude-code-adapter`, `client-subscriptions` |
| `palette-search-notify` | Paleta, búsqueda y notificaciones | M | `tui-views-actions` |
| `e2e-v0-3-tui` | E2E del TUI | S | `needs-input-inbox`, `palette-search-notify` |

**No entra:** lo asignado a otras fases en §43.

## Criterios de aceptación

### Scenario: Done de la fase
- **Given** todos los milestones de la fase mergeados
- **When** se ejecuta su milestone `e2e-*` (si existe) y la verificación manual de la release
- **Then** se cumple: Operación cotidiana posible sin salir del TUI.

## Referencias

- `docs/codefabric-spec.md` §43, §43.1
- `docs/adr/0002-implementation-decisions.md`

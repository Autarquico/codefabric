---
codename: codefabric
title: Fase 0 — Skeleton
type: epic
status: active
priority: P0
size: XL
issue: 2
prs: []
related_issues: [2]
sub_issues: [3, 4, 5, 6]
depends_on: []
supersedes: []
related_specs: [codefabric-spec]
created: 2026-10-10
updated: 2026-10-10
---

# Fase 0 — Skeleton

## Contexto / problema

Fase 0 del roadmap de `docs/codefabric-spec.md` (§43). Release objetivo: **v0.1**.

**Done de la fase:** `cf status` habla con un Controller persistente sobre TLS autenticado.

## Scope

**Entra:** los milestones de la tabla. Cada uno tiene su propia spec con scenarios.

| Spec | Título | Size | Depende de |
|------|--------|------|------------|
| `domain-core` | Domain core: entidades, máquinas de estado y errores | M | — |
| `controller-store` | Controller store: SQLite, transacciones, eventos e idempotencia | M | `domain-core` |
| `controller-identity-tls` | Identidad del Controller, TLS y tokens de cliente | M | `controller-store` |
| `api-sdk-cli-status` | API, Client SDK y `cf status` | M | `controller-identity-tls` |

`repo-bootstrap` (P0-1) ya está entregado en el PR #1.

**No entra:** lo asignado a otras fases en §43.

## Criterios de aceptación

### Scenario: Done de la fase
- **Given** todos los milestones de la fase mergeados
- **When** se ejecuta su milestone `e2e-*` (si existe) y la verificación manual de la release
- **Then** se cumple: `cf status` habla con un Controller persistente sobre TLS autenticado.

## Referencias

- `docs/codefabric-spec.md` §43, §43.1
- `docs/adr/0002-implementation-decisions.md`

---
codename: codefabric
title: "Paleta, búsqueda y notificaciones"
type: task
status: draft
priority: P1
size: M
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [tui-views-actions]
supersedes: []
related_specs: [codefabric-spec, phase-3-tui]
created: 2026-10-10
updated: 2026-10-10
---

# Paleta, búsqueda y notificaciones

## Contexto / problema

Milestone de la [Fase 3 — TUI](phase-3-tui.md). Implementa §19.6, §37, §26 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: frontend-eng y backend-eng.

## Scope

**Entra:**
- Paleta `Ctrl+P` (fuzzy) y modo `:` sobre el árbol Cobra
- `GET /v1/search` con FTS5 (`search_index` en las mismas transacciones); `cf search`
- `internal/app/notify`: dedupe, preferencias, notificación nativa y hooks

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Paleta
- **Given** la paleta abierta
- **When** se busca «drain»
- **Then** aparece `worker drain`

### Scenario: Modo :
- **Given** `:task cancel x`
- **When** se ejecuta
- **Then** mismo efecto y exit que el CLI

### Scenario: Búsqueda
- **Given** un short ID o prefijo ULID
- **When** se busca
- **Then** se resuelve

### Scenario: Dedupe
- **Given** el mismo `RunFailed` repetido
- **When** se notifica
- **Then** una sola vez

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §19.6, §37, §26

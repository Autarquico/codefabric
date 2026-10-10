---
codename: codefabric
title: "Suscripciones del Client SDK"
type: task
status: draft
priority: P1
size: S
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [observe-attach-events]
supersedes: []
related_specs: [codefabric-spec, phase-3-tui]
created: 2026-10-10
updated: 2026-10-10
---

# Suscripciones del Client SDK

## Contexto / problema

Milestone de la [Fase 3 — TUI](phase-3-tui.md). Implementa §31.4, §17.3 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- `internal/client/subscribe.go`: SSE con reconexión por cursor, snapshot + delta, `CF_CURSOR_EXPIRED`

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Reconexión
- **Given** un corte de red de 10 s
- **When** se reanuda
- **Then** sin huecos ni duplicados por `Sequence`

### Scenario: Cursor expirado
- **Given** un cursor fuera de retention
- **When** se suscribe
- **Then** `CF_CURSOR_EXPIRED` y resincronización

### Scenario: Carga
- **Given** 10 clientes suscritos
- **When** se emiten eventos
- **Then** latencia < 2 s p95

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §31.4, §17.3

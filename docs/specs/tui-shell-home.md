---
codename: codefabric
title: "TUI: shell y home"
type: task
status: draft
priority: P1
size: M
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [client-subscriptions]
supersedes: []
related_specs: [codefabric-spec, phase-3-tui]
created: 2026-10-10
updated: 2026-10-10
---

# TUI: shell y home

## Contexto / problema

Milestone de la [Fase 3 — TUI](phase-3-tui.md). Implementa §19.1–19.3 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: frontend-eng.

## Scope

**Entra:**
- `internal/tui`: modelo raíz, router, theme, keys, componentes, vista home; `cf` sin argumentos y con TTY abre el TUI

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Home
- **Given** datos de ejemplo
- **When** se abre el home
- **Then** contadores y Activity coinciden con el golden

### Scenario: Activity
- **Given** un evento nuevo
- **When** llega
- **Then** aparece en Activity

### Scenario: Controller caído
- **Given** el Controller inaccesible
- **When** se abre el TUI
- **Then** banner sin crash y reconexión

### Scenario: Models sin cuota
- **Given** provider sin cuota reportada
- **When** se muestra MODELS
- **Then** usage etiquetado, no porcentajes

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §19.1–19.3

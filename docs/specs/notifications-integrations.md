---
codename: codefabric
title: "Notificaciones e integraciones"
type: task
status: draft
priority: P2
size: M
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [palette-search-notify, promote]
supersedes: []
related_specs: [codefabric-spec, phase-10-elastic]
created: 2026-10-10
updated: 2026-10-10
---

# Notificaciones e integraciones

## Contexto / problema

Milestone de la [Fase 10 — Elastic workers e integrations](phase-10-elastic.md). Implementa §26, §12.5 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- ntfy, Telegram, Slack, SMTP; promote con estrategia `pr` vía API de GitHub desde el Worker; webhooks de CI

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Dedupe
- **Given** eventos repetidos
- **When** se notifican
- **Then** una sola vez

### Scenario: Preferencias
- **Given** preferencias por proyecto
- **When** se notifica
- **Then** se respetan

### Scenario: PR
- **Given** promote con estrategia `pr`
- **When** se ejecuta
- **Then** el PR solo se crea con confirmación

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §26, §12.5

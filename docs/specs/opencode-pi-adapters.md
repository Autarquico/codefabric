---
codename: codefabric
title: "Adapters de OpenCode y Pi"
type: task
status: draft
priority: P2
size: M
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [harness-adapter-framework]
supersedes: []
related_specs: [codefabric-spec, phase-10-elastic]
created: 2026-10-10
updated: 2026-10-10
---

# Adapters de OpenCode y Pi

## Contexto / problema

Milestone de la [Fase 10 — Elastic workers e integrations](phase-10-elastic.md). Implementa §7.3 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- Ver scenarios.

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Contrato
- **Given** los adapters de OpenCode y Pi
- **When** se ejecuta la suite de contrato
- **Then** pasan y declaran las capacidades ausentes como `unsupported`

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §7.3

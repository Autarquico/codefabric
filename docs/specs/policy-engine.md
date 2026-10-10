---
codename: codefabric
title: "Policy engine"
type: task
status: draft
priority: P2
size: L
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [secrets-local, confirmations-sessions-permissions]
supersedes: []
related_specs: [codefabric-spec, phase-9-policies]
created: 2026-10-10
updated: 2026-10-10
---

# Policy engine

## Contexto / problema

Milestone de la [Fase 9 — Policies, budgets y scheduler avanzado](phase-9-policies.md). Implementa §16.1, §16.2, §16.8 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- Tabla `policies` (YAML allow/deny); evaluador puro integrado en elegibilidad y `explain`; garantías declaradas por el Runtime; `cf policy list|set|explain`

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Deny
- **Given** un Worker con `deny: production`
- **When** se programa un Run de un Project production
- **Then** no es elegible

### Scenario: Aislamiento
- **Given** un Run que exige `network_deny`
- **When** solo hay Workers tmux
- **Then** no es elegible en ninguno

### Scenario: Audit
- **Given** un cambio de policy
- **When** se aplica
- **Then** queda auditado con antes y después

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §16.1, §16.2, §16.8

---
codename: codefabric
title: "Concurrencia, quotas y límites de modelo"
type: task
status: draft
priority: P2
size: M
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [policy-engine, provider-anthropic-openai]
supersedes: []
related_specs: [codefabric-spec, phase-9-policies]
created: 2026-10-10
updated: 2026-10-10
---

# Concurrencia, quotas y límites de modelo

## Contexto / problema

Milestone de la [Fase 9 — Policies, budgets y scheduler avanzado](phase-9-policies.md). Implementa §10.5, §10.2 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- Ver scenarios.

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Límite por cuenta
- **Given** límite de 2 Runs por cuenta de Claude
- **When** se lanza el tercero
- **Then** queda `queued` con su razón

### Scenario: Quota
- **Given** quota de CPU por Project
- **When** se programan Runs
- **Then** se respeta en todos los Workers

### Scenario: Límite de modelo
- **Given** cuota reportada > 95 %
- **When** se aplica `preserve-model-limits`
- **Then** se evita ese modelo

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §10.5, §10.2

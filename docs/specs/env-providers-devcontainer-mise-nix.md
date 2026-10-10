---
codename: codefabric
title: "Environment providers devcontainer, mise y nix"
type: task
status: draft
priority: P2
size: M
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [docker-environment-runtime]
supersedes: []
related_specs: [codefabric-spec, phase-10-elastic]
created: 2026-10-10
updated: 2026-10-10
---

# Environment providers devcontainer, mise y nix

## Contexto / problema

Milestone de la [Fase 10 — Elastic workers e integrations](phase-10-elastic.md). Implementa §25.3 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- Ver scenarios.

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Digest
- **Given** cada provider
- **When** se calcula el digest
- **Then** es estable por provider

### Scenario: Prepare idempotente
- **Given** un `prepare` ya ejecutado
- **When** se repite
- **Then** no cambia nada

### Scenario: Fallo
- **Given** un `prepare` que falla
- **When** se prepara el Run
- **Then** `failed(environment)`

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §25.3

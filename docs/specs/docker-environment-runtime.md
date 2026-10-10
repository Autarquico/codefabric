---
codename: codefabric
title: "Environment y Runtime docker"
type: task
status: draft
priority: P2
size: L
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [policy-engine, repo-config-environment-host]
supersedes: []
related_specs: [codefabric-spec, phase-10-elastic]
created: 2026-10-10
updated: 2026-10-10
---

# Environment y Runtime docker

## Contexto / problema

Milestone de la [Fase 10 — Elastic workers e integrations](phase-10-elastic.md). Implementa §25, §16.8 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng y devops-eng.

## Scope

**Entra:**
- Environment provider y Runtime docker que declaran `fs_scope`, `network_deny`, `resource_limits`; shell-out al CLI de docker

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Sin red
- **Given** un Run con `network_deny`
- **When** se programa
- **Then** solo es elegible en Workers docker y no tiene red de verdad

### Scenario: Recursos
- **Given** `resource_limits`
- **When** se ejecuta
- **Then** se aplican cgroups

### Scenario: Attach
- **Given** un Run docker interactive
- **When** se hace attach
- **Then** funciona

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §25, §16.8

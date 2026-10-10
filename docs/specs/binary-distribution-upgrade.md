---
codename: codefabric
title: "Distribución de binarios y upgrade"
type: task
status: draft
priority: P2
size: M
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [service-units]
supersedes: []
related_specs: [codefabric-spec, phase-10-elastic]
created: 2026-10-10
updated: 2026-10-10
---

# Distribución de binarios y upgrade

## Contexto / problema

Milestone de la [Fase 10 — Elastic workers e integrations](phase-10-elastic.md). Implementa §9.6, §41 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: devops-eng y backend-eng.

## Scope

**Entra:**
- `/v1/binaries/{os}/{arch}` con checksum; `cf worker upgrade [<w>|--all]` con reemplazo atómico y restart sin afectar Runs

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Runs vivos
- **Given** Runs activos
- **When** se hace upgrade
- **Then** siguen vivos

### Scenario: Checksum
- **Given** un checksum incorrecto
- **When** se descarga
- **Then** se rechaza

### Scenario: N-2
- **Given** un Worker N-2
- **When** se registra
- **Then** `CF_VERSION_INCOMPATIBLE` y `degraded`

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §9.6, §41

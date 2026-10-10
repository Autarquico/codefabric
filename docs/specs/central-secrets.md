---
codename: codefabric
title: "Backend central de secrets"
type: task
status: draft
priority: P2
size: M
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [secrets-local]
supersedes: []
related_specs: [codefabric-spec, phase-10-elastic]
created: 2026-10-10
updated: 2026-10-10
---

# Backend central de secrets

## Contexto / problema

Milestone de la [Fase 10 — Elastic workers e integrations](phase-10-elastic.md). Implementa §16.7 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- `SecretStore` cifrado con age en el Controller, entregado solo con lease vigente; backend `op` opcional

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Lease
- **Given** un secret central
- **When** se resuelve
- **Then** solo con lease vigente

### Scenario: Backup
- **Given** un backup
- **When** se inspecciona
- **Then** excluye la clave

### Scenario: Rotación
- **Given** una rotación de clave
- **When** se ejecuta
- **Then** documentada y testeada

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §16.7

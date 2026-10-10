---
codename: codefabric
title: "Worker provisioner"
type: task
status: draft
priority: P2
size: L
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [binary-distribution-upgrade, budgets-core]
supersedes: []
related_specs: [codefabric-spec, phase-10-elastic]
created: 2026-10-10
updated: 2026-10-10
---

# Worker provisioner

## Contexto / problema

Milestone de la [Fase 10 — Elastic workers e integrations](phase-10-elastic.md). Implementa §30, §16.3 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng y devops-eng.

## Scope

**Entra:**
- `internal/adapters/provisioner/{script,proxmox}`; disparo por presión de cola sujeto a budget y confirmación; ciclo completo hasta destroy

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Confirmación
- **Given** presión de cola y budget
- **When** se decide provisionar
- **Then** `workers.provision` pide confirmación

### Scenario: Destroy
- **Given** un efímero inactivo
- **When** pasa el tiempo
- **Then** drain y destroy

### Scenario: Sin budget
- **Given** budget agotado
- **When** hay presión de cola
- **Then** no se provisiona

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §30, §16.3

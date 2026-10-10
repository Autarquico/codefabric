---
codename: codefabric
title: "Atribución de usage"
type: task
status: draft
priority: P1
size: M
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [claude-code-adapter, codex-adapter, logs-output-storage]
supersedes: []
related_specs: [codefabric-spec, phase-6-models-usage]
created: 2026-10-10
updated: 2026-10-10
---

# Atribución de usage

## Contexto / problema

Milestone de la [Fase 6 — Models/usage](phase-6-models-usage.md). Implementa §14.1, §14.3–14.6 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- `model_usage_events` atribuido por `run_id`; agregación con drill-down §14.5; pricing solo para cuentas por API; `cf models usage`

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Desglose
- **Given** usage en 2 Workers
- **When** se agrupa por Worker
- **Then** la suma coincide con el total

### Scenario: Suscripción
- **Given** una suscripción
- **When** se muestra
- **Then** tokens sin coste, etiqueta «Fabric-attributed»

### Scenario: No observable
- **Given** un Run sin usage observable
- **When** se muestra
- **Then** como no observable, nunca como cero

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §14.1, §14.3–14.6

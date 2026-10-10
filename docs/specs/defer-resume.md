---
codename: codefabric
title: "Defer y resume"
type: task
status: draft
priority: P1
size: M
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [checkpoint-capture]
supersedes: []
related_specs: [codefabric-spec, phase-5-handoff]
created: 2026-10-10
updated: 2026-10-10
---

# Defer y resume

## Contexto / problema

Milestone de la [Fase 5 — Checkpoint/handoff](phase-5-handoff.md). Implementa §11.4, §11.7 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- `POST /v1/tasks/{ref}/defer|resume`; `cf task defer|resume [--worker]`; reescritura de ruta del cwd en Claude

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Defer
- **Given** un Run activo
- **When** se ejecuta defer
- **Then** Run `suspended(deferred)` y Task `waiting(deferred)`

### Scenario: Resume
- **Given** una Task diferida
- **When** se ejecuta resume
- **Then** un Run nuevo `continues` desde el checkpoint, nunca el mismo Run

### Scenario: Major distinto
- **Given** `native_state` de otro major del adapter
- **When** se reanuda
- **Then** solo source + summary, con aviso

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §11.4, §11.7

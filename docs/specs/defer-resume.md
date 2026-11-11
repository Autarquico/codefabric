---
codename: codefabric
title: Defer y resume
type: task
status: active
priority: P1
size: M
issue: 161
prs: []
related_issues: [161]
sub_issues: []
depends_on: [checkpoint-capture]
supersedes: []
related_specs: [codefabric-spec, phase-5-handoff]
created: 2026-10-10
updated: 2026-11-11
---

# Defer y resume

## Contexto / problema

Milestone de la [Fase 5 — Checkpoint/handoff](phase-5-handoff.md). Implementa §11.4, §11.7 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- `POST /v1/tasks/{ref}/defer|resume`; `cf task defer|resume [--worker]`; `cf do --worker <w>` (fija el Worker vía `Requirements.Workers`)
- `Prepare` del Worker desde el relay (hoy solo trae de `origin`): un Run que continúa un checkpoint o un snapshot ve sus commits sin push a `origin`
- Semilla de sesión del cliente: `cf do|task resume --snapshot --session <id>` lleva una sesión local de Claude al Run que va a correr (`lineage.native_seed: {adapter, version, digest}`); sin Run sintético
- Tras un `TaskPulled` posterior al último checkpoint, `resume` se rechaza salvo `--force` o semilla del mismo cliente (enmienda de §11.7: doble escritor).
  «Mismo cliente» = la identidad del certificado de cliente que emitió `TaskPulled`. La semilla o `--force` consumen el marcador; `--force` se audita.
- Un jsonl con patrón sensible bloquea la semilla salvo `--allow-sensitive-session`

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

### Scenario: Resume en otro Worker
- **Given** una Task diferida en el Worker A con commits solo en el relay
- **When** `resume --worker B`
- **Then** el Run en B parte del commit del checkpoint sin que nada se publique en `origin`

### Scenario: Semilla de sesión
- **Given** una sesión local de Claude cerrada y un repo dirty
- **When** `cf do --snapshot --session <id> --worker B`
- **Then** el Run en B reanuda esa sesión sobre el snapshot y el árbol local queda intacto

### Scenario: Semilla tras pull propio
- **Given** un `TaskPulled` de este cliente
- **When** `cf task resume --worker w --snapshot --session <id>`
- **Then** un Run `continues` con `native_seed` y el marcador queda consumido

### Scenario: Resume tras pull
- **Given** un `TaskPulled` posterior al último checkpoint
- **When** `cf task resume` sin `--force`
- **Then** error con la explicación y nada cambia

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §11.4, §11.7

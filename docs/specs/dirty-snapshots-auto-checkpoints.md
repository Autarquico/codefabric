---
codename: codefabric
title: Snapshots dirty y checkpoints automáticos
type: task
status: active
priority: P1
size: M
issue: 162
prs: []
related_issues: [162]
sub_issues: []
depends_on: [checkpoint-capture]
supersedes: []
related_specs: [codefabric-spec, phase-5-handoff]
created: 2026-10-10
updated: 2026-11-11
---

# Snapshots dirty y checkpoints automáticos

## Contexto / problema

Milestone de la [Fase 5 — Checkpoint/handoff](phase-5-handoff.md). Implementa §22.2, §23.2 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- `cf do --snapshot`: commit temporal en el cliente y push a `refs/cf/snapshots/*`; checkpoints por intervalo y pre-destructivos
- Relay para clientes: todo cliente autenticado del Controller accede a los Projects de ese Controller (no hay scoping por Project en el modelo, §16.8); lectura de la rama de la Task y de `refs/cf/checkpoints/*`, y create-only en `refs/cf/snapshots/<digest>`; ningún otro ref
- El snapshot y `cf do` registran `source.branch` (la rama local de partida)

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Snapshot
- **Given** un workspace dirty
- **When** se crea una Task con `--snapshot`
- **Then** se registra el digest y el árbol local queda intacto

### Scenario: Intervalo
- **Given** intervalo de 10 min
- **When** pasa el tiempo
- **Then** se crean checkpoints periódicos

### Scenario: Pre-destructivo
- **Given** `drain --force`
- **When** se ejecuta
- **Then** checkpoint previo si el harness lo permite; el Run capturado queda suspendido (`end_reason: drained`) y se puede continuar con `cf task resume --worker X` o mover, y los Runs cuyo checkpoint falla o no llega a tiempo se cancelan

### Scenario: Ref fuera de la lista
- **Given** un cliente autenticado
- **When** empuja a un ref fuera de `refs/cf/snapshots/<digest>` o a un Project inexistente
- **Then** rechazo y nada cambia

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §22.2, §23.2

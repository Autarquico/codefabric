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
- Exclusiones en snapshots (enmienda): el snapshot del cliente aplica `checkpoint.exclude` del Controller, igual que la captura de un Worker. El cliente lo lee de `GET /v1/checkpoint/exclude` y, si no puede, no toma el snapshot (no cae a los defaults). Un archivo no rastreado que casa no entra en el commit y el cliente lo lista; uno rastreado que casa conserva la versión de HEAD. El commit parte de HEAD (`read-tree`), así que lleva los commits sin empujar.
- Retención de snapshots (enmienda): `refs/cf/snapshots/<commit>` lo recolecta el GC cuando todas las Tasks cuyos Runs parten de ese commit (`runs.source_commit`, `tasks.source_snapshot`) llevan archivadas `retention.relay_branches` y ningún Run con publicación pendiente lo usa. Un snapshot que ningún Run nombra (reintento con otro commit por `request_id`) no se recolecta: seguimiento.
- Idempotencia de `cf task resume --snapshot` (enmienda): el commit del snapshot no entra en la clave de idempotencia; repetir con el mismo `request_id` devuelve el Run de la primera llamada, con su primer snapshot, sin crear otro.

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
- **Then** checkpoint previo si el harness lo permite

### Scenario: Ref fuera de la lista
- **Given** un cliente autenticado
- **When** empuja a un ref fuera de `refs/cf/snapshots/<digest>` o a un Project inexistente
- **Then** rechazo y nada cambia

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §22.2, §23.2

---
codename: codefabric
title: Registro de Workers y heartbeats
type: task
status: active
priority: P0
size: M
issue: 9
prs: [33]
related_issues: [9]
sub_issues: []
depends_on: [api-sdk-cli-status]
supersedes: []
related_specs: [codefabric-spec, phase-1-workers]
created: 2026-10-10
updated: 2026-10-10
---

# Registro de Workers y heartbeats

## Contexto / problema

Milestone de la [Fase 1 — Workers y ejecución básica](phase-1-workers.md). Implementa §3.5, §5.3–5.4, §9.1, §9.2, §9.3, §9.5, §9.6, §16.6, §16.8, §41 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- `internal/app/workers`: máquina de estados y sweeper de TTL
- `internal/worker/daemon`: register, heartbeat, sysinfo; `epoch` nuevo en cada arranque
- Migración `0102_workers.sql`
- Middleware de alcance: los principals Worker solo acceden a `/v1/worker/*`
- CLI: `cf worker join <invite>` (no interactivo), `cf worker run|list|show|drain|undrain|remove`; `cf worker run` cableado en `cmd/cf`

**No entra:**
- Wizard interactivo de join (doctor-and-join-wizard)

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Alta
- **Given** una invitación de Worker
- **When** se ejecutan `join` y `run`
- **Then** pasa de `joining` a `online` y `cf worker list` muestra recursos y versión

### Scenario: Offline
- **Given** 45 s sin heartbeat
- **When** pasa el sweeper
- **Then** el Worker queda `offline` con `WorkerLost`; al volver el heartbeat recupera su estado y emite `WorkerReconnected`

### Scenario: Versión incompatible
- **Given** un Worker fuera de N-1
- **When** se registra
- **Then** queda `degraded` y no elegible

### Scenario: Alcance de identidades
- **Given** un certificado de Worker / de cliente
- **When** llama a `/v1/tasks` / `/v1/worker/*`
- **Then** se rechaza en ambos casos

### Scenario: Baja
- **Given** un Worker con Runs activos / drenado y vacío
- **When** se ejecuta `remove`
- **Then** se rechaza / pasa a `removed` y su certificado se revoca

### Scenario: Epoch
- **Given** un daemon en marcha
- **When** se reinicia
- **Then** el `epoch` cambia

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §3.5, §5.3–5.4, §9.1, §9.2, §9.3, §9.5, §9.6, §16.6, §16.8, §41

---
codename: codefabric
title: "Almacenamiento de logs y output"
type: task
status: draft
priority: P1
size: M
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [observe-attach-events]
supersedes: []
related_specs: [codefabric-spec, phase-4-recovery]
created: 2026-10-10
updated: 2026-10-10
---

# Almacenamiento de logs y output

## Contexto / problema

Milestone de la [Fase 4 — Logs, recovery y reconciliation](phase-4-recovery.md). Implementa §3.12, §20.3, §20.5, §34 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- `POST /v1/worker/logs` por chunks; output en blobs gzip con `logs_index`; logs estructurados en tabla; `cf logs` con todos los filtros

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Filtros
- **Given** logs de varios Workers
- **When** se filtra por worker, level y since
- **Then** resultado correcto

### Scenario: Follow
- **Given** `--follow`
- **When** llegan líneas
- **Then** aparecen en < 2 s

### Scenario: Volumen
- **Given** 100 MB de output
- **When** se pagina
- **Then** sin cargarlo todo en memoria

### Scenario: Orden
- **Given** relojes desincronizados
- **When** se listan
- **Then** orden por `Sequence`

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §3.12, §20.3, §20.5, §34

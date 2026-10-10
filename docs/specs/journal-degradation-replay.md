---
codename: codefabric
title: "Degradación del journal y replay"
type: task
status: draft
priority: P1
size: M
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [logs-output-storage]
supersedes: []
related_specs: [codefabric-spec, phase-4-recovery]
created: 2026-10-10
updated: 2026-10-10
---

# Degradación del journal y replay

## Contexto / problema

Milestone de la [Fase 4 — Logs, recovery y reconciliation](phase-4-recovery.md). Implementa §1.3, §20.4, §27.3 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- Tres etapas de §1.3, compactación de usage, `JournalTruncated`, backlog visible en `cf status`

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Etapa 1
- **Given** journal en `max_size`
- **When** sigue llegando output
- **Then** se descarta primero raw output y después logs

### Scenario: Etapa 2
- **Given** events + usage siguen creciendo
- **When** se supera el umbral
- **Then** el usage se compacta y el Worker queda `degraded`

### Scenario: Etapa 3
- **Given** `hard_max`
- **When** se alcanza
- **Then** `JournalTruncated` con rango y los eventos de estado de Run se conservan

### Scenario: Replay (e2e #4)
- **Given** una partición de 10 min
- **When** vuelve la red
- **Then** replay completo y ordenado

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §1.3, §20.4, §27.3

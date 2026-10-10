---
codename: codefabric
title: "Captura de checkpoints"
type: task
status: draft
priority: P1
size: L
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [claude-code-adapter, codex-adapter, logs-output-storage]
supersedes: []
related_specs: [codefabric-spec, phase-5-handoff]
created: 2026-10-10
updated: 2026-10-10
---

# Captura de checkpoints

## Contexto / problema

Milestone de la [Fase 5 — Checkpoint/handoff](phase-5-handoff.md). Implementa §11.4, §11.7, §23.1, §23.3, §22.4, §16.8 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- `internal/app/checkpoints` con resumen mecánico
- Quiesce = checkpoint nativo confirmado o stop del grupo completo de procesos (cubre hijos)
- `native_state`: jsonl de sesión (Claude), rollout (Codex)
- Refs `refs/cf/checkpoints/*`; migración `0501_checkpoints.sql`; `cf run checkpoint create|list`

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Exclusiones
- **Given** `.env` y `*.pem` en el worktree
- **When** se captura
- **Then** no entran y se listan como excluidos

### Scenario: Captura coherente
- **Given** fakeharness escribiendo sin parar
- **When** se captura
- **Then** solo tras parar

### Scenario: Fallo con harness vivo
- **Given** la captura falla con el harness vivo
- **When** se procesa
- **Then** el Run vuelve a su estado previo con `RunCheckpointFailed`

### Scenario: Exit gana
- **Given** un exit durante `checkpointing`
- **When** se arbitra
- **Then** gana el exit

### Scenario: Cancel
- **Given** un cancel durante `checkpointing`
- **When** se procesa
- **Then** la captura se aborta; si ya estaba publicada se conserva

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §11.4, §11.7, §23.1, §23.3, §22.4, §16.8

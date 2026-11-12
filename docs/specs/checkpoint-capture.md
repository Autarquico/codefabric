---
codename: codefabric
title: Captura de checkpoints
type: task
status: active
priority: P1
size: L
issue: 160
prs: [167]
related_issues: [160]
sub_issues: []
depends_on: [claude-code-adapter, codex-adapter, logs-output-storage]
supersedes: []
related_specs: [codefabric-spec, phase-5-handoff]
created: 2026-10-10
updated: 2026-11-11
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
- Identidad de sesión determinista: `--session-id <uuid5(run:epoch)>` en el primer `Start`; si la sesión ya existe, `--resume <id>` (spike: repetir `--session-id` falla con `already in use`)
- `harness.SessionPorter` (`Locate/Export/Import`) para Claude: `native_state` = tar.gz `{jsonl, subagents/, manifest.json}`; reescritura JSON por línea solo de `cwd` y `sessionId`
- `native_state` opt-in por Project; guard de valores conocidos del Run y bloqueo por patrones sensibles
- Fase `checkpointing` persistida en el state.db del Worker; la captura va antes de `settle`, que hoy borra el ConfigDir del Run

**No entra:**
- `native_state` de Codex en v0.4: se degrada a source + summary con aviso `CF_CHECKPOINT_UNSUPPORTED`.
- Redactar el jsonl (podría romper bloques firmados): se bloquea y decide el usuario.

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

### Scenario: Sesión tras settle
- **Given** un Run de Claude con `native_state` activado
- **When** se captura y el Run se asienta
- **Then** el blob de `native_state` existe con manifest válido; que su jsonl reanude con `--resume` se comprueba con `CF_REQUIRE_CLAUDE=1`

### Scenario: Secreto en la sesión
- **Given** un jsonl que contiene un valor de secreto del Run
- **When** se captura
- **Then** no se sube `native_state`, el checkpoint queda con source + summary y se avisa con las líneas afectadas

### Scenario: native_state desactivado
- **Given** un Project sin `native_state`
- **When** se captura
- **Then** solo source + summary

### Scenario: Start idempotente
- **Given** un `Start` repetido del mismo Run y epoch con la sesión ya creada
- **When** se arranca
- **Then** usa `--resume` con el mismo id y no falla

## Notas técnicas / riesgos

- Spike 2026-11-11 (claude 2.1.319 Mac → 2.1.322 Linux): una sesión cortada en un `tool_use` sin resultado, con `sessionId` y `cwd` reescritos, en otro SO, con `CLAUDE_CONFIG_DIR` aislado y `CLAUDE_CODE_OAUTH_TOKEN`, reanuda conservando el contexto. No verificado: token de otra cuenta.

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §11.4, §11.7, §23.1, §23.3, §22.4, §16.8

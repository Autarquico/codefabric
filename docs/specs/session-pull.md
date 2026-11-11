---
codename: codefabric
title: Traer la Task al checkout local
type: task
status: active
priority: P1
size: L
issue: 163
prs: []
related_issues: [163]
sub_issues: []
depends_on: [defer-resume, dirty-snapshots-auto-checkpoints]
supersedes: []
related_specs: [codefabric-spec, phase-5-handoff, checkpoint-capture]
created: 2026-11-11
updated: 2026-11-11
---

# Traer la Task al checkout local

## Contexto / problema

Milestone de la [Fase 5 — Checkpoint/handoff](phase-5-handoff.md). Cierra la vuelta del flujo
Mac → Worker → Mac: la máquina del usuario no es un Worker, así que retomar en ella necesita traer
la rama y la sesión de Claude al checkout local.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- `cf task pull <task> [--worktree <path>] [--no-session] [--defer] [--open]`: trae la rama del último Run
  desde el relay, escribe la sesión en el directorio real de Claude e imprime el `claude --resume <id>` exacto;
  `--open` lo lanza.
- Evento auditado `TaskPulled`.
- Si el `lineage.native_seed` del Run procede de este cliente, la sesión vuelve con su `sessionId` original (regla prefijo/`.bak`) y se borra el marcador `handed-off`; si no, id nuevo.
- Los cambios sin commitear del Worker llegan como el commit de sellado del checkpoint; la salida lo dice.
- Rama local: fast-forward de la rama de partida si es ancestro; si no, `cf/<task>`; si `cf/<task>` diverge,
  `cf/<task>@<run-short>` y aviso. Nunca reset ni stash automáticos; sin checkout con el árbol dirty.

**No entra:**
- Codex en v0.4.
- Merge automático ni escritura en `origin`.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Clean
- **Given** una Task `waiting(deferred)` y el repo local limpio
- **When** `cf task pull`
- **Then** si la Task nació con `source.branch` y es ancestro, esa rama avanza; si no, `cf/<task>`; checkout hecho y se imprime el comando de resume

### Scenario: Run vivo
- **Given** un Run `running`
- **When** `pull` sin `--defer`
- **Then** error con sugerencia y nada cambia

### Scenario: Rama de partida no ancestro
- **Given** `source.branch` local con commits que no están en el checkpoint
- **When** `pull`
- **Then** la rama de partida no se toca y se usa `cf/<task>`

### Scenario: Run vivo con --defer
- **Given** un Run `running`
- **When** `pull --defer`
- **Then** defer completo y luego pull del checkpoint nuevo

### Scenario: Sin sesión
- **Given** una Task con `native_state`
- **When** `pull --no-session`
- **Then** rama traída, `~/.claude` intacto y sin comando de resume

### Scenario: Vuelta de una semilla propia
- **Given** una Task sembrada desde este cliente con la sesión `s`
- **When** `pull`
- **Then** la sesión local `s` se actualiza (prefijo o `.bak`) y el marcador `handed-off` desaparece

### Scenario: Dirty
- **Given** cambios sin commitear
- **When** `pull`
- **Then** ni checkout ni stash; la rama se crea, la salida ofrece `--worktree` y los archivos quedan idénticos

### Scenario: Worktree
- **Given** el árbol dirty
- **When** `pull --worktree ../x`
- **Then** worktree nuevo y la sesión se registra para esa ruta (realpath)

### Scenario: Rama local divergente
- **Given** `cf/<task>` local con commits que no están en el checkpoint
- **When** `pull`
- **Then** rama `cf/<task>@<run>` y ningún commit se pierde

### Scenario: Sesión local prefijo
- **Given** un jsonl local con ese id que es prefijo del entrante
- **When** `pull`
- **Then** se sustituye atómicamente dejando `.bak`

### Scenario: Sesión divergente
- **Given** un jsonl local con ese id que no es prefijo del entrante
- **When** `pull`
- **Then** no se sobrescribe; se escribe con `sessionId` nuevo y se avisa

### Scenario: Sesión abierta
- **Given** `~/.claude/sessions/*` activo con ese id
- **When** `pull`
- **Then** error y nada cambia

### Scenario: Excluidos
- **Given** un checkpoint con `.env` excluido
- **When** `pull`
- **Then** se listan los excluidos y los `.env` locales no se tocan

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Reutiliza `client/session/` (localizar, detectar viva, importar) con `claude-handto`.
- macOS case-insensitive y `/tmp` → `/private/tmp`: `EvalSymlinks` antes de codificar la ruta del proyecto.

## Referencias

- `docs/codefabric-spec.md` §11.7, §22.2, §23.1
- `.claude/loop/phase-5/plan.md` §5.1

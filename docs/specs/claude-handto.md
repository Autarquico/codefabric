---
codename: codefabric
title: "/handto desde Claude Code"
type: task
status: draft
priority: P1
size: M
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [defer-resume, dirty-snapshots-auto-checkpoints]
supersedes: []
related_specs: [codefabric-spec, phase-5-handoff, session-pull]
created: 2026-11-11
updated: 2026-11-11
---

# /handto desde Claude Code

## Contexto / problema

Milestone de la [Fase 5 — Checkpoint/handoff](phase-5-handoff.md). Dentro de una sesión de Claude Code
abierta en la máquina del usuario, `/handto <worker>` transfiere esa sesión (contexto y repo, también lo no
commiteado) a ese Worker. `cf` instala lo que Claude Code necesita para ello.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- `cf handoff --session <id> --to <worker>`: sin Task ligada a la sesión, `cf do --snapshot --session <id> --worker <w>`;
  con Task ligada (registro local `session → task`), `cf task resume --worker <w> --snapshot --session <id>`.
  Imprime la Task, el Worker y `cf attach <task>`.
- Sesión viva propia: se permite cuando el id es el de la sesión que llama; el export corta el jsonl tras el
  último `tool_use` (la propia llamada). Tras el éxito, marcador local `handed-off`.
- `cf claude install [--project] [--force]`, `cf claude uninstall`, `cf claude status`: escriben la skill embebida
  en el binario en `~/.claude/skills/handto/SKILL.md` (o `.claude/skills/` del repo con `--project`), marcada con
  la versión de `cf`. No sobrescriben una skill `handto` ajena sin `--force`.
- `cf setup` (rol client) ofrece instalarla; `cf doctor` del cliente avisa si falta o es de otra versión.
- Skill `handto`: `disable-model-invocation: true`, `argument-hint: <worker>`, `arguments: [worker]`,
  `allowed-tools` limitado a `cf handoff` y `cf worker list`; usa `${CLAUDE_SESSION_ID}`; en éxito pide cerrar con
  `/exit` y seguir con `cf attach`; en error muestra el código `CF_*` y no reintenta.

**No entra:**
- Codex en v0.4.
- `/handto` dentro de una sesión que ya corre en un Worker (Worker → Worker es `cf task move`).
- Plugin o marketplace de Claude Code.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Handto
- **Given** una sesión abierta en un repo del Project y el Worker `w` online con `claude/oauth-token`
- **When** `/handto w`
- **Then** una Task `running` en `w` con la sesión sembrada; lo no commiteado viaja por snapshot, el árbol local queda intacto y se imprime `cf attach`

### Scenario: Worker desconocido
- **Given** ningún Worker llamado `foo` u offline
- **When** `/handto foo`
- **Then** error `CF_*` con la lista de Workers y ninguna Task creada

### Scenario: Segundo handto
- **Given** el marcador `handed-off` de esa sesión
- **When** `/handto otro`
- **Then** aviso de doble escritor; solo con `--force`, y entonces `resume` en el otro Worker, no una Task nueva

### Scenario: Turno en curso
- **Given** el jsonl acaba en el `tool_use` de `cf handoff`
- **When** se exporta
- **Then** se corta ahí y el Worker reanuda sin error

### Scenario: Secreto en la sesión
- **Given** un jsonl con un patrón sensible
- **When** `/handto w`
- **Then** bloqueo con las líneas y la opción explícita para continuar

### Scenario: Install idempotente
- **Given** la skill instalada de la misma versión
- **When** `cf claude install`
- **Then** sin cambios; de otra versión la actualiza; una ajena da error sin `--force`

### Scenario: Ida y vuelta
- **Given** `/handto w` y trabajo en `w`
- **When** `cf task defer` y `cf task pull --open` en la máquina del usuario
- **Then** la sesión recuerda lo dicho antes del handto y ve los commits de `w`

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001): la integración vive en `internal/integrations/claudecode/` y se compone en `cmd/cf`.
- Spike 2026-11-11: `${CLAUDE_SESSION_ID}` y `$worker` se sustituyen en una skill; una sesión cortada en un `tool_use` sin resultado reanuda en otro SO.
- La skill escribe en `~/.claude` del usuario y sube su sesión: review de security obligatoria.

## Referencias

- `docs/codefabric-spec.md` §11.7, §22.2
- `.claude/loop/phase-5/plan.md` revisión v3
- https://code.claude.com/docs/en/skills

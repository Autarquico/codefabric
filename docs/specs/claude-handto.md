---
codename: codefabric
title: /handto desde Claude Code
type: task
status: shipped
priority: P1
size: M
issue: 164
prs: [166, 172, 171]
related_issues: [164]
sub_issues: []
depends_on: [defer-resume, dirty-snapshots-auto-checkpoints]
supersedes: []
related_specs: [codefabric-spec, phase-5-handoff, session-pull]
created: 2026-11-11
updated: 2026-11-13
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
  El Run es interactivo (`--interactive`) y su prompt de continuación y el nombre de la Task salen de la sesión
  (último mensaje del usuario, truncado). Imprime la Task, el Worker y `cf attach <task>`.
- Sesión viva propia: se permite cuando el id es el de la sesión que llama; el export corta el jsonl tras el
  último `tool_use` (la propia llamada) solo si no le sigue un `tool_result`. (Enmienda, lab 2026-11-13) Claude
  Code 2.1.319 aún no ha escrito la llamada cuando `cf` corre: si la última llamada tiene resultado, o no hay
  ninguna, el jsonl va entero; cortar en una anterior perdería su resultado y los turnos posteriores. Tras el
  éxito, marcador local `handed-off`. (Enmienda) "la que llama" se verifica: el proceso `claude` dueño de la sesión (`~/.claude/sessions/<pid>.json` con ese `sessionId`) debe ser
  ancestro del proceso de `cf` (cadena de ppid, `/proc` o `ps`); si no, la sesión está abierta en otra ventana y se
  rechaza pidiendo cerrarla. En Codex la sesión que llama es la que dice `CODEX_SESSION_ID`.
- (Enmienda, lab 2026-11-13) Destino: un Worker `online` o `degraded`; el scheduler decide si lo degradado afecta
  al harness de la sesión.
- (Enmienda, lab 2026-11-13) Si la Task ligada está cancelada o archivada y ninguno de sus Runs sigue vivo, la
  sesión se desliga y se libera el marcador sin `--force`: se trata como una sesión nueva, y la Task nueva lleva un
  sufijo de su request para no chocar con el nombre de la terminada. Un Run aún parándose mantiene la guarda.
- (Enmienda, lab 2026-11-13) Una petición que el Controller rechaza (request inválida, no encontrada, policy,
  transición) no se reenvía en el reintento; cualquier otro error puede ser una respuesta perdida y se repite igual.
- (Enmienda) Con Task ligada, el resume lleva snapshot solo si el árbol (por tree de git) cambió respecto al último
  handoff; si no, continúa del último checkpoint de la Task.
- (Enmienda) `--force` y `--allow-sensitive-session` exigen TTY: Claude Code ejecuta Bash sin terminal y la skill no
  puede levantar esas guardas; el error imprime el comando exacto para el terminal del usuario.
- (Enmienda) Codex entra: `cf codex install|uninstall|status` instala la skill `handto` en `~/.agents/skills`
  (`$handto <worker>`), y `cf handoff` lee la sesión de `CODEX_SESSION_ID`.
- `cf claude install [--scope user|project] [--force] [--dry-run]`, `cf claude uninstall`, `cf claude status`:
  escriben la skill embebida en el binario en `~/.claude/skills/handto/SKILL.md` (o `.claude/skills/` del repo con
  `--scope project`), marcada con la versión de `cf`; reutilizan el instalador de skills de `cf mcp install` y su
  vocabulario de flags. No sobrescriben una skill `handto` ajena sin `--force`.
- `cf setup` (rol client) ofrece instalarla; `cf doctor` del cliente avisa si falta o es de otra versión.
- Skill `handto`: `disable-model-invocation: true`, `argument-hint: <worker>`, `arguments: [worker]`,
  `allowed-tools` limitado a `cf handoff` y `cf worker list`; usa `${CLAUDE_SESSION_ID}`; en éxito pide cerrar con
  `/exit` y seguir con `cf attach`; en error muestra el código `CF_*` y no reintenta.

**No entra:**
- `/handto` dentro de una sesión que ya corre en un Worker (Worker → Worker es `cf task move`).
- Plugin o marketplace de Claude Code.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Handto
- **Given** una sesión abierta en un repo del Project y el Worker `w` online con `claude/oauth-token`
- **When** `/handto w`
- **Then** un Run interactivo `running` en `w` con la sesión sembrada; lo no commiteado viaja por snapshot, el árbol local queda intacto, se imprime `cf attach` y `cf attach` muestra la sesión reanudada

### Scenario: Worker desconocido
- **Given** ningún Worker llamado `foo`, o uno que no está `online` ni `degraded`
- **When** `/handto foo`
- **Then** error `CF_*` con la lista de Workers y ninguna Task creada

### Scenario: Segundo handto
- **Given** el marcador `handed-off` de esa sesión
- **When** `/handto otro`
- **Then** aviso de doble escritor; solo con `--force`, y entonces `resume` en el otro Worker, no una Task nueva.
  Si la Task ligada está cancelada o archivada sin Runs vivos, no hay aviso: Task nueva con nombre sufijado

### Scenario: Turno en curso
- **Given** el jsonl acaba en el `tool_use` de `cf handoff`
- **When** se exporta
- **Then** se corta ahí y el Worker reanuda sin error

### Scenario: Secreto en la sesión
- **Given** un jsonl con un patrón sensible
- **When** `/handto w`
- **Then** bloqueo con las líneas; solo `--allow-sensitive-session` continúa

### Scenario: Install idempotente
- **Given** la skill instalada de la misma versión
- **When** `cf claude install`
- **Then** sin cambios; de otra versión la actualiza; una ajena da error sin `--force`

### Scenario: Ida y vuelta
- **Given** `/handto w` y trabajo en `w`
- **When** `cf task defer` y `cf task pull --open` en la máquina del usuario
- **Then** la sesión, con su id original, recuerda lo dicho antes del handto y ve los commits de `w`; el marcador `handed-off` desaparece

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001): la integración vive en `internal/integrations/claudecode/` y se compone en `cmd/cf`.
- Spike 2026-11-11: `${CLAUDE_SESSION_ID}` y `$worker` se sustituyen en una skill; una sesión cortada en un `tool_use` sin resultado reanuda en otro SO.
- La skill escribe en `~/.claude` del usuario y sube su sesión: review de security obligatoria.

## Referencias

- `docs/codefabric-spec.md` §11.7, §22.2
- `.claude/loop/phase-5/plan.md` revisión v3
- https://code.claude.com/docs/en/skills

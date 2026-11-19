---
codename: codefabric
title: Adapter de Claude Code
type: task
status: shipped
priority: P0
size: L
issue: 20
prs: [64]
related_issues: [20]
sub_issues: []
depends_on: [harness-adapter-framework]
supersedes: []
related_specs: [codefabric-spec, phase-2-harnesses-workflows]
created: 2026-10-10
updated: 2026-10-31
---

# Adapter de Claude Code

## Contexto / problema

Milestone de la [Fase 2 — Claude/Codex y Workflows](phase-2-harnesses-workflows.md). Implementa §7.2–7.4, §8.12, §16.7 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- Detección e instalación (dry-run)
- Componentes: `settings.json`, `CLAUDE.md`, `commands/`, `agents/`, `skills/`, `output-styles/`; de `.claude.json` solo `mcpServers`
- `CLAUDE_CONFIG_DIR` por Run con enlace a credenciales; fallback `CLAUDE_CODE_OAUTH_TOKEN` en macOS (riesgo R2.1)
- Arranque headless `claude -p --output-format stream-json --verbose --permission-mode bypassPermissions --disallowedTools AskUserQuestion`; interactive en PTY
- Parser del stream (init, usage, result); README con los 8 puntos de §7.3
- Needs-input no soportado en Fase 2: `input_required` → stop (§10.7) → `failed` → sellado

**No entra:**
- Input estructurado (needs-input-inbox, Fase 3)

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Snapshot
- **Given** un HOME fixture
- **When** se llama a `SnapshotConfig`
- **Then** componentes y digests estables, sin credenciales

### Scenario: Apply aislado
- **Given** un apply sobre el directorio del Run
- **When** se compara `~/.claude` antes y después
- **Then** no cambia

### Scenario: Stream
- **Given** un stream-json grabado
- **When** se procesa con `Events`
- **Then** salen `RunStarted` con modelo, el usage y el exit mapeado

### Scenario: Input requerido
- **Given** headless detecta espera de input
- **When** se procesa
- **Then** stop local, `failed(input_required)` y sellado tras `RunStopped`

### Scenario: Respuesta de un Run headless
- **Given** un Run headless cuyo `result` trae texto
- **When** el Worker lee el stream de eventos
- **Then** el texto (`Summary`) queda como respuesta del Run, acotada a 8 KiB, y llega al Controller en el `exited` si este anuncia `run_answer`

### Scenario: Run headless que no sale tras acabar
- **Given** un Run headless de Claude Code cuyo último evento es un `result` sin error, con el input ya cerrado (`input.eof`), sin prompts abiertos, y cuyo proceso sigue vivo
- **When** ni `events.jsonl` ni el stderr capturado crecen durante `IdleAfterResult` (120 s por defecto)
- **Then** el Worker lo detiene con el stop normal (SIGTERM al grupo, SIGKILL tras la gracia) y reporta `exited` con `end_reason: idle_after_result` y el exit real; el Controller completa el Run con su respuesta, o, si no anuncia `idle_after_result`, el Worker no envía el motivo y el Run se resuelve por su exit
- Cualquier crecimiento, un prompt o un evento que no sea `result` reinicia la cuenta; un `result` con `is_error` nunca se detiene por inactividad; una salida natural o un stop previo (usuario) mantienen su resolución. La cuenta se persiste en el cursor de eventos del Worker y sobrevive a un reinicio del daemon. Sólo aplica a adapters con la capacidad `IdleAfterResult` (Claude Code headless). Un Run que ya estaba atascado antes de actualizar no se recoge (el cursor migrado empieza con `result_ok` en falso): se cancela con `cf task cancel`. El motivo sólo se conserva si, tras el último drenaje, el cursor sigue en `result_ok` sin prompt abierto y el marcador de salida del wrapper registra `cause: stop`.

### Scenario: Live
- **Given** «create hello.txt» con Claude real
- **When** se ejecuta
- **Then** el Run acaba `completed` y `published`

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §7.2–7.4, §8.12, §16.7

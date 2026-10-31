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

### Scenario: Live
- **Given** «create hello.txt» con Claude real
- **When** se ejecuta
- **Then** el Run acaba `completed` y `published`

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §7.2–7.4, §8.12, §16.7

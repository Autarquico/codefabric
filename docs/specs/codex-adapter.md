---
codename: codefabric
title: Adapter de Codex CLI
type: task
status: active
priority: P0
size: M
issue: 21
prs: [45]
related_issues: [21]
sub_issues: []
depends_on: [harness-adapter-framework]
supersedes: []
related_specs: [codefabric-spec, phase-2-harnesses-workflows]
created: 2026-10-10
updated: 2026-10-28
---

# Adapter de Codex CLI

## Contexto / problema

Milestone de la [Fase 2 — Claude/Codex y Workflows](phase-2-harnesses-workflows.md). Implementa §7.2–7.4, §8.12, §16.7 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- `CODEX_HOME` por Run; `config.toml` parseado para convertir `env`/headers en refs
- Render de las refs `{{cf-secret:<ref>}}` de `config.toml` en la config materializada del Run, con escape TOML y limpieza recuperable (sale de secrets-local, decisión del usuario 2026-10-16)
- Componentes: `AGENTS.md`, `prompts/`, `skills/`; enlace a `auth.json`
- Arranque `codex exec --json --sandbox workspace-write` con approvals `never`; parser JSONL

**No entra:**
- Input estructurado (spike en Fase 3)

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Snapshot
- **Given** un fixture
- **When** se hace snapshot
- **Then** `config.toml` sin valores de `env` (convertidos en refs) y sin `auth.json`

### Scenario: Apply aislado
- **Given** un apply
- **When** se compara `~/.codex`
- **Then** no cambia

### Scenario: Usage
- **Given** un JSONL grabado
- **When** se procesa
- **Then** usage normalizado

### Scenario: Refresh concurrente
- **Given** dos Runs que refrescan el token
- **When** se ejecutan a la vez
- **Then** `auth.json` no se corrompe

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §7.2–7.4, §8.12, §16.7

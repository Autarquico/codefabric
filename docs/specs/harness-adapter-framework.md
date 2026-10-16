---
codename: codefabric
title: Framework de adapters de harness
type: task
status: active
priority: P0
size: M
issue: 18
prs: [42]
related_issues: [18]
sub_issues: []
depends_on: [worker-executor-tmux, git-relay-publication]
supersedes: []
related_specs: [codefabric-spec, phase-2-harnesses-workflows]
created: 2026-10-10
updated: 2026-10-16
---

# Framework de adapters de harness

## Contexto / problema

Milestone de la [Fase 2 — Claude/Codex y Workflows](phase-2-harnesses-workflows.md). Implementa §7.1–7.4, §8.12, §3.3, §42.3 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- `internal/adapters/harness`: interfaz de §7.2, `HarnessCapabilities`, `Host`, `RunSpec`, registro
- `harnesstest`: suite de contrato común (§42.3)
- `internal/worker/host` y `internal/testutil/fakeharness` (binario scriptable)
- Materialización en `runs/<id>/config/`; elegibilidad por modos soportados (`CF_HARNESS_UNAVAILABLE`)

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Capacidad ausente
- **Given** un adapter con `Resume=false`
- **When** se llama a `Resume`
- **Then** devuelve `unsupported`, nunca datos inventados

### Scenario: Modo no soportado
- **Given** un Run interactive para un adapter solo headless
- **When** se evalúa la elegibilidad
- **Then** `CF_HARNESS_UNAVAILABLE` en elegibilidad y en `explain`, no en runtime

### Scenario: Aislamiento
- **Given** dos Runs con workflows distintos y `ConfigIsolation`
- **When** se materializan
- **Then** cada uno ve su propio digest de configuración

### Scenario: Contrato
- **Given** la suite de contrato
- **When** se ejecuta contra fakeharness
- **Then** todo pasa

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §7.1–7.4, §8.12, §3.3, §42.3

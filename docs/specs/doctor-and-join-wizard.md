---
codename: codefabric
title: Doctor y wizard de join
type: task
status: active
priority: P0
size: M
issue: 25
prs: [73, 75]
related_issues: [25]
sub_issues: []
depends_on:
  - claude-code-adapter
  - codex-adapter
  - workflow-apply-drift-lock
  - secrets-local
  - observe-attach-events
supersedes: []
related_specs: [codefabric-spec, phase-2-harnesses-workflows]
created: 2026-10-10
updated: 2026-11-01
---

# Doctor y wizard de join

## Contexto / problema

Milestone de la [Fase 2 — Claude/Codex y Workflows](phase-2-harnesses-workflows.md). Implementa §9.1, §9.4, §9.5, §36.2–36.3, §44.17 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng y frontend-eng.

## Scope

**Entra:**
- `internal/worker/doctor`: conexión, runtime, git, disco, harness (`Detect` + `ValidateConfig`), workflow materializado, auth presente sin revelarla, capabilities, aviso de bypass sin aislamiento
- Salud por componente en el heartbeat; `degraded` por componente
- CLI: `cf doctor`, `cf worker doctor`, `cf worker join` interactivo, `cf setup`

**Entrega en dos PRs:** 25-A (#73: doctor del Worker, salud por componente en el heartbeat y en el scheduler, `cf worker doctor`, `cf doctor`; scenarios Login ausente, Disco, Verde y Sin SSH) y 25-B (`cf worker join` interactivo con instalación de harnesses y `cf setup`).

**No entra:**
- Nada fuera de lo indicado en la epic.
- El rol Controller no forma parte de `cf setup` (D9): `cf setup` lo remite a `cf controller init`.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Login ausente
- **Given** Claude sin login en un Worker
- **When** se ejecuta doctor
- **Then** rojo con la acción «claude login en <worker>»; el Worker queda `degraded` solo para claude-code

### Scenario: Disco
- **Given** disco por debajo del umbral
- **When** se ejecuta doctor
- **Then** `degraded` y `explain` lo indica

### Scenario: Verde
- **Given** doctor en verde
- **When** se lanza una Task compatible
- **Then** se ejecuta

### Scenario: Sin SSH
- **Given** un cliente sin SSH al Worker
- **When** se ejecuta `cf doctor`
- **Then** avisa de que attach no funcionará

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §9.1, §9.4, §9.5, §36.2–36.3, §44.17

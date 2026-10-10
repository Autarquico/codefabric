---
codename: codefabric
title: "Projects, Tasks y Runs: API y CLI"
type: task
status: active
priority: P0
size: M
issue: 8
prs: [32]
related_issues: [8]
sub_issues: []
depends_on: [api-sdk-cli-status]
supersedes: []
related_specs: [codefabric-spec, phase-1-workers]
created: 2026-10-10
updated: 2026-10-10
---

# Projects, Tasks y Runs: API y CLI

## Contexto / problema

Milestone de la [Fase 1 — Workers y ejecución básica](phase-1-workers.md). Implementa §3.1, §3.3, §3.4, §3.13, §11.1, §11.2, §11.7, §18.2 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng y frontend-eng.

## Scope

**Entra:**
- `internal/app/{projects,tasks,runs}`; congelación lógica al crear el Run (`spec_digest` con JSON canónico + sha256)
- Migración `0101_tasks_runs.sql` con estado de Task materializado vía `DeriveTaskState`
- `internal/client/gitctx`: inferir Project por el remote del cwd y resolver `source.ref` con `git ls-remote` en el cliente (D1)
- Endpoints de projects, tasks, runs y `/v1/resolve`; defer, resume, move, fork y promote devuelven `CF_NOT_IMPLEMENTED`
- CLI: `cf project …`, `cf task new|list|show|cancel|complete|archive|set`, `cf do`, `cf run list|show`

**No entra:**
- `repo_config` (llega en repo-config-environment-host)
- Plans y dependencias (Fase 8)

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Crear trabajo
- **Given** un cwd cuyo remote coincide con el Project `backend`
- **When** se ejecuta `cf do "x"`
- **Then** se crean la Task y un Run `queued` con `spec_digest` y commit congelados; sin coincidencia, error y nunca un default silencioso

### Scenario: Sin arranque
- **Given** `cf task new --no-start`
- **When** se consulta la Task
- **Then** queda `waiting(start)`

### Scenario: Congelación
- **Given** un Run `queued`
- **When** cambian los defaults del Project
- **Then** `spec_digest` del Run no cambia

### Scenario: Operaciones futuras
- **Given** una Task
- **When** se ejecuta `cf task defer`
- **Then** `CF_NOT_IMPLEMENTED` y exit 8

### Scenario: Unicidad de nombre
- **Given** una Task `x` no archivada / archivada
- **When** se crea otra con el mismo name
- **Then** error de unicidad / se permite

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §3.1, §3.3, §3.4, §3.13, §11.1, §11.2, §11.7, §18.2

---
codename: codefabric
title: "Domain core: entidades, máquinas de estado y errores"
type: task
status: shipped
priority: P0
size: M
issue: 3
prs: [28]
related_issues: [3]
sub_issues: []
depends_on: []
supersedes: []
related_specs: [codefabric-spec, phase-0-skeleton]
created: 2026-10-10
updated: 2026-10-10
---

# Domain core: entidades, máquinas de estado y errores

## Contexto / problema

Milestone de la [Fase 0 — Skeleton](phase-0-skeleton.md). Implementa §2, §3, §3.13, §9.5, §11.3, §11.7, §20.1–20.2, §40, Apéndice A de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- `internal/domain`: IDs `<kind>_<ULID>`, short IDs y resolución de refs (§3.13)
- Structs de Project, Plan, Task, Run, Worker, Workflow, Lease, Checkpoint, Artifact (§3)
- `NextRunState(from, event)` con la tabla completa de §11.7
- `DeriveTaskState(TaskView)` con las 14 filas de §11.7
- Máquina de estados del Worker (§9.5)
- Tipos de evento (§20.1) y struct `Event` (§20.2)
- `Error{Code, Message, Details}` y `ExitCode(Code)` según §40
- Defaults del Apéndice A y `ValidateRun` (invariantes cruzados: no existe `completed` con `publication: none`)

**No entra:**
- Gating por fase (`CF_NOT_IMPLEMENTED`): vive en la capa app
- Persistencia

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Estados terminales
- **Given** un Run en cualquier estado terminal
- **When** se aplica cualquier evento con `NextRunState`
- **Then** devuelve `CF_INVALID_TRANSITION`

### Scenario: Derivación exhaustiva de Task
- **Given** un generador de todas las combinaciones de `TaskView`
- **When** se llama a `DeriveTaskState`
- **Then** siempre encaja exactamente una fila, nunca hay panic, y las filas 1–14 tienen casos golden

### Scenario: Invariante de publicación
- **Given** un Run `completed` con `publication: none`
- **When** se valida con `ValidateRun`
- **Then** devuelve error

### Scenario: Refs ambiguas
- **Given** dos Tasks candidatas con el name `auth-race`
- **When** se resuelve la ref
- **Then** devuelve `CF_AMBIGUOUS_REF` con la lista de candidatos; un prefijo ULID de 5 caracteres no resuelve

### Scenario: Exit codes
- **Given** cada código `CF_*`
- **When** se llama a `ExitCode`
- **Then** coincide con la tabla de §40

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §2, §3, §3.13, §9.5, §11.3, §11.7, §20.1–20.2, §40, Apéndice A

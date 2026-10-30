---
codename: codefabric
title: Config del repo y Environment host
type: task
status: shipped
priority: P0
size: M
issue: 24
prs: [58, 60]
related_issues: [24]
sub_issues: []
depends_on: [git-relay-publication, secrets-local]
supersedes: []
related_specs: [codefabric-spec, phase-2-harnesses-workflows]
created: 2026-10-10
updated: 2026-10-30
---

# Config del repo y Environment host

## Contexto / problema

Milestone de la [Fase 2 — Claude/Codex y Workflows](phase-2-harnesses-workflows.md). Implementa §3.1, §3.3, §25.1–25.3, §10.1 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- `cf project explain`: merge de config del Controller, `.codefabric/project.yaml` y restricciones de policy
- `project.yaml` se lee del commit congelado con `git show` y se valida antes de ejecutar scripts
- `internal/adapters/environment/host`: `Prepare`, servicios con sonda `ready`, digest sobre inputs del commit
- Migración `0204_environments.sql`; CLI `cf environment apply|list|show`

**No entra:**
- Providers docker/devcontainer/mise/nix (Fase 10)

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Clave no permitida
- **Given** una clave no permitida en `project.yaml`
- **When** se prepara el Run
- **Then** `failed` con `CF_REPO_CONFIG_INVALID` sin ejecutar scripts

### Scenario: Límite de recursos
- **Given** el repo pide más recursos que el límite
- **When** se resuelve la config
- **Then** se aplica el límite del Controller

### Scenario: Prepare fallido
- **Given** `prepare` falla
- **When** se prepara el Run
- **Then** `failed(environment)`, `publication: none` y reserva liberada tras `RunStopped(never_started)`

### Scenario: Digest
- **Given** cambios en `digest_inputs`
- **When** se calcula el digest
- **Then** cambia

### Scenario: Explain
- **Given** un Project
- **When** se ejecuta `cf project explain`
- **Then** muestra el origen de cada campo

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §3.1, §3.3, §25.1–25.3, §10.1

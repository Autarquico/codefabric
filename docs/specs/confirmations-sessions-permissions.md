---
codename: codefabric
title: "Confirmaciones, sesiones MCP y permisos"
type: task
status: draft
priority: P2
size: M
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [projects-tasks-runs-api]
supersedes: []
related_specs: [codefabric-spec, phase-8-mcp-plans]
created: 2026-10-10
updated: 2026-10-10
---

# Confirmaciones, sesiones MCP y permisos

## Contexto / problema

Milestone de la [Fase 8 — MCP y Plans](phase-8-mcp-plans.md). Implementa §16.3, §16.5, §16.6 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- Tabla `confirmations` vinculada a operación, payload, recursos con versión, actor y `request_id`; aprobar ejecuta una vez revalidando; `CF_CONFIRMATION_STALE`; expiración
- `mcp_sessions` con token de sesión y perfil aplicado en el servidor; `cf approve|deny`

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Requiere confirmación
- **Given** `runs.promote` con perfil `execute`
- **When** se llama
- **Then** `CF_CONFIRMATION_REQUIRED` con id

### Scenario: Aprobar ejecuta
- **Given** una confirmación pendiente
- **When** se aprueba
- **Then** se ejecuta una vez y se emite `ConfirmationResolved` con el resultado

### Scenario: Stale
- **Given** un recurso cambiado antes de aprobar
- **When** se aprueba
- **Then** `CF_CONFIRMATION_STALE`

### Scenario: Mismo request_id
- **Given** la llamada original
- **When** se repite
- **Then** devuelve el estado actual

### Scenario: Perfil read
- **Given** perfil `read`
- **When** se llama a `tasks.create`
- **Then** `CF_POLICY_DENIED`

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §16.3, §16.5, §16.6

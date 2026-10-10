---
codename: codefabric
title: "Servidor MCP"
type: task
status: draft
priority: P2
size: L
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [plans-dag, confirmations-sessions-permissions, client-subscriptions]
supersedes: []
related_specs: [codefabric-spec, phase-8-mcp-plans]
created: 2026-10-10
updated: 2026-10-10
---

# Servidor MCP

## Contexto / problema

Milestone de la [Fase 8 — MCP y Plans](phase-8-mcp-plans.md). Implementa §17 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- `internal/mcp` con el go-sdk oficial por stdio; tools de §17.2 mapeadas 1:1 al SDK; `request_id` obligatorio en mutantes; `events.wait` por long-poll (máx. 10 min)

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Master (e2e #9)
- **Given** un master model
- **When** llama a `tasks.create` y `events.wait{RunCompleted}`
- **Then** recibe el evento y `runs.get` devuelve el resultado

### Scenario: Cursor expirado
- **Given** un cursor expirado
- **When** llama a `events.wait`
- **Then** `CF_CURSOR_EXPIRED`

### Scenario: Sin shell
- **Given** el listado de tools
- **When** se inspecciona
- **Then** no hay tool de shell ni `secrets.read`

### Scenario: request_id
- **Given** una tool mutante sin `request_id`
- **When** se llama
- **Then** error de validación

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §17

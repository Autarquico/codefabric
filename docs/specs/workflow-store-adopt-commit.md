---
codename: codefabric
title: "Workflows: almacenamiento, adopt y commit"
type: task
status: active
priority: P0
size: L
issue: 19
prs: []
related_issues: [19]
sub_issues: []
depends_on: [projects-tasks-runs-api, harness-adapter-framework]
supersedes: []
related_specs: [codefabric-spec, phase-2-harnesses-workflows]
created: 2026-10-10
updated: 2026-10-10
---

# Workflows: almacenamiento, adopt y commit

## Contexto / problema

Milestone de la [Fase 2 — Claude/Codex y Workflows](phase-2-harnesses-workflows.md). Implementa §8.1–8.3, §8.6, §8.9–8.11, §21.3 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng y frontend-eng.

## Scope

**Entra:**
- `internal/app/workflows`: versiones con CAS sobre parent, refs `@N|@latest|@sha256`
- Migración `0201_workflows.sql`; manifest con `schema_version`, componentes con digest y ownership
- `internal/client/workflowadopt` (en el cliente): snapshot, escáner de secretos, subida de blobs, commit
- Endpoints de workflows y blobs
- CLI: `cf workflow list|show|adopt|commit [--rebase]|history`, con checklist interactivo y flags equivalentes

**No entra:**
- Overlays y `merged` (Fase 9)

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Adopt
- **Given** `~/.claude` y `~/.codex`
- **When** se ejecuta `adopt` con settings, skills y MCP
- **Then** se crea v1 con componentes y digests, sin auth

### Scenario: Secretos en settings
- **Given** `GITHUB_TOKEN=ghp_…` en los settings
- **When** se adopta
- **Then** se rechaza o se sustituye por una secret ref tras confirmación; el snapshot no contiene el valor

### Scenario: Conflicto
- **Given** dos commits concurrentes con el mismo parent
- **When** llegan al Controller
- **Then** uno gana y el otro recibe `CF_WORKFLOW_CONFLICT` (exit 4)

### Scenario: Congelación
- **Given** un Run creado con `@latest`
- **When** se hace commit de una versión nueva
- **Then** el Run conserva la versión y digest con los que se creó

### Scenario: Digest estable
- **Given** el mismo contenido
- **When** se calcula el digest
- **Then** es el mismo

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §8.1–8.3, §8.6, §8.9–8.11, §21.3

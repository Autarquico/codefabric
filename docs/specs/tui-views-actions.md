---
codename: codefabric
title: "TUI: vistas y acciones"
type: task
status: draft
priority: P1
size: L
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [tui-shell-home]
supersedes: []
related_specs: [codefabric-spec, phase-3-tui]
created: 2026-10-10
updated: 2026-10-10
---

# TUI: vistas y acciones

## Contexto / problema

Milestone de la [Fase 3 — TUI](phase-3-tui.md). Implementa §19.4, §38 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: frontend-eng.

## Scope

**Entra:**
- Vistas Tasks, Workers, Workflows, Runs, Projects, Logs
- `internal/cli/actions`: registro de descriptores compartido por TUI y paleta
- Attach vía `tea.ExecProcess`; `GET /v1/tasks/{ref}/timeline` derivado de eventos

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Attach
- **Given** una Task
- **When** se pulsa attach
- **Then** el TUI se suspende y vuelve al cerrar

### Scenario: Errores
- **Given** una acción fallida
- **When** se muestra
- **Then** código y acción recuperable

### Scenario: Paridad CLI
- **Given** el registro de acciones
- **When** se recorre en un test
- **Then** cada acción tiene un comando Cobra existente

### Scenario: Timeline
- **Given** una Task
- **When** se abre su timeline
- **Then** coincide con los eventos

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §19.4, §38

---
codename: codefabric
title: Needs-input e Inbox
type: task
status: shipped
priority: P1
size: L
issue: 97
prs: [110, 119, 121]
related_issues: [97]
sub_issues: []
depends_on: [claude-code-adapter, client-subscriptions]
supersedes: []
related_specs: [codefabric-spec, phase-3-tui]
created: 2026-10-10
updated: 2026-11-06
---

# Needs-input e Inbox

## Contexto / problema

Milestone de la [Fase 3 — TUI](phase-3-tui.md). Implementa §11.6, §19.5, §20.1 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng y frontend-eng.

## Scope

**Entra:**
- Habilitar `running⇄waiting_input` y la fila 5 de §11.7
- Claude: `--input-format stream-json` por FIFO + `--permission-prompt-tool` contra `cf worker mcp-bridge` (socket unix por Run)
- Codex: `StructuredInput=false` hasta validar `codex app-server` (spike con time-box)
- Orden `input`; `POST /v1/tasks/{ref}/respond`, `GET /v1/inbox`; CLI `cf task respond`, `cf inbox`; vista Inbox

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Permiso
- **Given** Claude headless pide permiso
- **When** se detecta
- **Then** `waiting_input`, `TaskNeedsInput` y la Task aparece en el inbox

### Scenario: Respuesta
- **Given** una Task esperando input
- **When** se ejecuta `cf task respond`
- **Then** se entrega, se emite `InputProvided` con el actor y el Run vuelve a `running`

### Scenario: No respondable
- **Given** un adapter sin `StructuredInput`
- **When** se responde
- **Then** `CF_RUN_NOT_RESPONDABLE`, exit 8 y sugerencia de attach

### Scenario: Idempotencia
- **Given** el mismo `request_id`
- **When** se repite respond
- **Then** el input se entrega una vez

### Scenario: Reinicio
- **Given** un daemon reiniciado en `waiting_input`
- **When** se responde
- **Then** funciona

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

- Permisos de Claude headless (decisión del usuario, 2026-11-04): opt-in por Task/Workflow con `permissions: ask`; por defecto se mantiene `bypassPermissions`. AskUserQuestion se habilita siempre que el Worker anuncie `structured-input:v1`.
- Los permisos se responden como resultado de la herramienta MCP `mcp__cf__approve` (`--mcp-config`); preguntas y texto libre como mensajes stream-json por stdin (FIFO).
- Codex: spike de 1 h sobre `codex app-server` (decisión del usuario, 2026-11-04); si es viable, spec aparte. En esta fase `StructuredInput=false`.
- Redacción del prompt (SEC-2): el Worker redacta `summary` y `options` con los needles de secretos del Run antes de reportar `input_requested` (T4b, con test: un needle en el input de una herramienta no llega al reporte, a los eventos ni al inbox). El Controller no tiene needles; sólo acota (tamaño, UTF-8, sin caracteres de control o formato —se rechaza, no se recorta—, máximo de prompts abiertos por Run y epoch).
- Un `input` encolado se retira (`cancelled_at`) en cuanto se encola un stop del lease, sea cual sea la causa; el Worker (T4b) además ignora input de un Run terminado.
- Inbox en esta fase: peticiones de input y fallos que requieren decisión; aprobaciones, conflictos de workflow y avisos de presupuesto quedan diferidos (sin productores).

- Límites aceptados de `permissions: ask` (2026-11-05):
  - El harness corre con el mismo usuario del SO que el Worker: puede alcanzar el socket del Run o el FIFO de entrada. `ask` revisa las llamadas a herramientas del modelo dentro del propio enforcement de Claude Code; no aísla al harness ni a los procesos que ya se le permitió lanzar.
  - El modo se fija con `--permission-mode default` y `--setting-sources user`, pero el snapshot del Workflow es la fuente «user» (bajo `CLAUDE_CONFIG_DIR`): sus allow rules y hooks siguen aplicando. El Workflow lo controla el owner, así que se acepta. Claude además aprueba por sí mismo comandos de solo lectura sin consultar al puente.
  - Un prompt retirado en el Worker (llamada MCP cancelada) sigue en el inbox hasta que termina el Run: avisar al Controller exige un tipo de informe nuevo (followup).

## Referencias

- `docs/codefabric-spec.md` §11.6, §19.5, §20.1

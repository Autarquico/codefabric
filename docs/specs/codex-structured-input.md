---
codename: codefabric
title: Input estructurado para Codex vía app-server
type: task
status: draft
priority: P2
size: L
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [needs-input-inbox, codex-adapter]
supersedes: []
related_specs: [codefabric-spec, needs-input-inbox]
created: 2026-11-05
updated: 2026-11-05
---

# Input estructurado para Codex vía app-server

## Contexto / problema

Sale del spike de 1 h de [needs-input-inbox](needs-input-inbox.md) (decisión del usuario, 2026-11-04). Hoy el adapter de Codex lanza `codex exec --json -c approval_policy="never"` y declara `StructuredInput=false`, así que `cf task respond` devuelve `CF_RUN_NOT_RESPONDABLE`.

`codex app-server` (probado en vivo con codex-cli 0.160.1, sin autenticar ni gastar tokens) habla JSON-RPC por JSONL sobre stdio y cubre lo que pide el inbox:

- aprobaciones como peticiones del servidor (`item/commandExecution|fileChange|permissions/requestApproval`, `item/tool/requestUserInput`), respondidas con `accept`, `acceptForSession`, `decline` o `cancel`, sin timeout documentado para comandos y ficheros;
- mensajes de seguimiento con `turn/start` (hilo parado) o `turn/steer` (turno en curso);
- reanudación por id con `thread/resume`.

Veredicto del spike: **viable con salvedades**; esfuerzo estimado de 4 a 6 días.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- Modo headless de Codex sobre `codex app-server` por stdio: un bridge dentro del wrapper (o de la sesión tmux) que posee el stdio del app-server, refleja la salida en `OutputPath`, mantiene la lista de peticiones pendientes en el directorio del Run y acepta las escrituras de `SendInput`. Sin dependencias Go nuevas.
- Segundo parser de eventos para las notificaciones de app-server (`item/*`, `turn/completed`, `thread/tokenUsage/updated`), distinto del de `exec --json`.
- `StructuredInput=true` y la capacidad `harness:codex:structured-input:v1` solo con una versión de Codex validada; `exec --json` sigue como camino por defecto hasta entonces.
- Mapeo de aprobaciones y preguntas a `input_requested` / respuestas del inbox.

**No entra:**
- El transporte WebSocket (`--listen unix://`), que exige una librería cliente.
- Servicios alojados o comerciales (ver riesgos).

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Aprobación
- **Given** un Run de Codex que pide aprobar un comando
- **When** se responde desde el inbox minutos después
- **Then** el comando se ejecuta o se rechaza según la respuesta y el Run sigue

### Scenario: Seguimiento
- **Given** un Run de Codex esperando input
- **When** se envía texto con `cf task respond --text`
- **Then** llega como turno nuevo o steer y el Run vuelve a `running`

### Scenario: Reinicio del Worker
- **Given** un Run de Codex esperando aprobación
- **When** se reinicia el daemon del Worker
- **Then** la aprobación pendiente sigue respondible (el app-server sobrevive al daemon)

## Notas técnicas / riesgos

- El subcomando está marcado `[experimental]`; el schema va ligado a cada versión del CLI y no hay política SemVer (p. ej. `thread/rollback` se eliminó). Fijar y validar la versión soportada.
- Una aprobación pendiente vive solo en la memoria del proceso app-server: si muere, solo queda una reanudación en frío del historial. Por eso el app-server tiene que sobrevivir al Worker.
- Sin verificar en el spike: una aprobación real respondida minutos después (requiere auth y un turno de modelo), qué pasa con una aprobación ordinaria sin cliente conectado, y si el replay de peticiones pendientes al reconectar está en 0.160.1.
- La documentación dice: "App-server authentication has never been permitted for commercial or hosted services." Leer los términos antes de ofrecer codefabric como servicio alojado.

## Referencias

- https://learn.chatgpt.com/docs/app-server
- https://raw.githubusercontent.com/openai/codex/main/codex-rs/app-server/README.md
- `internal/adapters/harness/codex`

---
codename: codefabric
title: "Insights de Runs y visor de conversación"
type: task
status: draft
priority: P2
size: M
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [analytics-history, usage-attribution, checkpoint-capture, models-tui]
supersedes: []
related_specs: [codefabric-spec, phase-7-experiments, phase-6-models-usage]
created: 2026-10-15
updated: 2026-10-15
---

# Insights de Runs y visor de conversación

## Contexto / problema

Milestone de la [Fase 7 — Experiments](phase-7-experiments.md). Amplía `analytics-history` (§39) y el drill-down de usage (§14.5) de `docs/codefabric-spec.md`.

Inspirado en [agentlytics](https://github.com/f/agentlytics), que analiza en local el historial de sesiones de varios editores y agentes. Aquí se aplica solo a los Runs que orquesta CodeFabric. Los datos ya pasan por el Controller (eventos, usage atribuido por `run_id`, output, `native_state` del checkpoint), pero hoy no se agregan para responder preguntas como estas:
- qué tools usa cada agente;
- cuándo y cómo se trabaja;
- qué combinación de harness y modelo rinde mejor;
- qué dijo el agente en un Run concreto.

Owner previsto en el bucle Develop: backend-eng (agregación y API) y frontend-eng (CLI y TUI).

## Scope

**Entra:**
- **Eventos de tool calls normalizados.** Los parsers de `claude-code` y `codex` emiten un evento `tool.call` por cada llamada: `run_id`, harness, nombre de la tool, resultado ok/error, duración y tamaño del input y del output. Nunca se guarda el contenido de los argumentos.
- **`cf insights`** (con `--json`):
  - `tools [--by harness|model|project] [--since]`: frecuencia de cada tool y tasa de error.
  - `activity [--since]`: Runs, Tasks completadas y tokens por día, por hora y por día de la semana.
  - `compare --by harness|model [--project] [--since]`: por cada combinación, tokens por Task completada, duración p50 y p95, tasa de éxito (reutiliza `analytics-history`) y tasa de error de tools.
- **`cf run transcript <ref> [--follow] [--json]`:** conversación del Run (mensajes de usuario y asistente, y tool calls con su resultado), normalizada a partir del stream del harness o del `native_state` del checkpoint, y pasada por `support/redact`.
- **TUI:**
  - vista Insights (tools, actividad y comparativa);
  - pestaña Transcript en el detalle del Run.
- **Endpoints:** `GET /v1/insights/{tools,activity,compare}` y `GET /v1/runs/{ref}/transcript`, con la misma autorización que el output del Run.

**No entra:**
- Importar sesiones hechas fuera de CodeFabric (Claude Code o Codex locales, otros editores).
- Costes estimados de suscripciones (§14.6): solo usage, y dólares cuando haya pricing de API.
- Compartir sesiones entre usuarios o un relay de equipo.
- Insights generados por un LLM.
- Exportación a PDF o CSV (`--json` cubre la integración).

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Frecuencia de tools
- **Given** streams grabados de Claude y Codex con tool calls, alguna con error
- **When** se ejecuta `cf insights tools --by harness`
- **Then** los recuentos y la tasa de error por tool coinciden con los streams, y no se guarda ningún argumento de tool

### Scenario: Capacidad ausente
- **Given** un harness cuyo adapter no expone tool calls
- **When** se piden los insights de tools
- **Then** ese harness aparece como `unsupported`, no con cero

### Scenario: Actividad
- **Given** un fixture de Runs repartidos en varios días y horas
- **When** se ejecuta `cf insights activity`
- **Then** las series por día, hora y día de la semana coinciden con los eventos, en la zona horaria del cliente

### Scenario: Comparativa
- **Given** Runs de dos combinaciones de harness y modelo sobre el mismo Project
- **When** se ejecuta `cf insights compare --by harness`
- **Then** cada fila muestra tokens por Task completada, p50/p95 de duración, tasa de éxito y tasa de error de tools; una muestra insuficiente se marca como tal; sin pricing no aparecen dólares

### Scenario: Transcript redactado
- **Given** un Run cuyo agente imprimió un secret autorizado
- **When** se ejecuta `cf run transcript <ref>`
- **Then** la conversación aparece completa y en orden, el secret como `[REDACTED]`, y las tool calls con su resultado

### Scenario: Transcript de un Run en curso
- **Given** un Run `running`
- **When** se ejecuta `cf run transcript <ref> --follow`
- **Then** los mensajes nuevos llegan en menos de 2 s p95, y cerrar el cliente no afecta al Run

### Scenario: Alcance
- **Given** un certificado de Worker, o un cliente revocado
- **When** pide `/v1/insights/*` o `/v1/runs/{ref}/transcript`
- **Then** se rechaza con el mismo código que `/v1/runs/{ref}/output` y no se filtra ningún dato

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.
- **Privacidad:** los argumentos de las tools pueden contener secretos o código. Los eventos `tool.call` guardan solo metadatos; el contenido vive en el transcript, que se redacta y sigue la retención del output (P4-3).
- **Formatos nativos cambiantes (R2.3):** el transcript se normaliza por adapter, con fixtures versionadas; lo que un adapter no sepa interpretar se muestra como bloque opaco, nunca se inventa.
- **Volumen:** las agregaciones por día y hora se calculan con índices o tablas materializadas, sin recorrer todos los eventos en cada consulta.
- Pendiente de decidir en el plan: si el transcript se materializa como blob al terminar el Run o se reconstruye bajo demanda.

## Referencias

- `docs/codefabric-spec.md` §14.4–14.6, §39
- [agentlytics](https://github.com/f/agentlytics) (referencia de producto, no de código)

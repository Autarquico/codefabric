---
codename: codefabric
title: API, Client SDK y `cf status`
type: task
status: active
priority: P0
size: S
issue: 6
prs: [31]
related_issues: [6]
sub_issues: []
depends_on: [controller-identity-tls]
supersedes: []
related_specs: [codefabric-spec, phase-0-skeleton]
created: 2026-10-10
updated: 2026-10-10
---

# API, Client SDK y `cf status`

## Contexto / problema

Milestone de la [Fase 0 — Skeleton](phase-0-skeleton.md). Implementa §18.1–18.2, §31.1–31.2, §34, §40 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng (API, SDK) y frontend-eng (CLI).

## Scope

**Entra:**
- `cf status` y `GET /v1/status` (versión, uptime, salud de la DB)
- `internal/controller/api`: middleware de `request_id`, `slog` y `recover`
- `cmd/cf` / `internal/cli`: flags globales `--controller` y `--debug`, y `--json` global con golden files
- Los 5 scenarios de abajo

**Ya entregado por [controller-identity-tls](controller-identity-tls.md)** (no se rehace aquí):
- `internal/api/v1`: DTOs de identidad y sobre de error `{error:{code,message,details}}`
- `internal/controller/api`: servidor TLS, middleware de auth con tabla de rutas y mapeo CF→HTTP
- `cf controller run` (el wiring del Controller vive en `internal/controller/daemon`, ADR 0001)
- `internal/client`: SDK base con TLS fijado y credenciales
- `internal/cli`: mapeo de errores a exit codes (`*domain.Error` → `domain.ExitCode`)

**No entra:**
- Recursos distintos de status

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Status
- **Given** un Controller corriendo y un cliente conectado
- **When** se ejecuta `cf status`
- **Then** muestra versión, uptime y salud de la DB; `--json` coincide con el golden

### Scenario: Controller caído
- **Given** el Controller parado
- **When** se ejecuta `cf status --json`
- **Then** stdout contiene `CF_CONTROLLER_UNREACHABLE` y el exit es 6

### Scenario: Persistencia (Done de Fase 0)
- **Given** un Controller reiniciado
- **When** se ejecuta `cf status`
- **Then** el ID del Controller y los datos persisten

### Scenario: Sin credencial
- **Given** un cliente sin credencial
- **When** se ejecuta `cf status`
- **Then** exit 6 y sugerencia de `cf connect`

### Scenario: Error interno
- **Given** un error inesperado del servidor
- **When** se ejecuta cualquier comando
- **Then** exit 1; sin detalles salvo con `--debug`, que muestra el `request_id` (y la causa solo si el fallo es local: red o TLS); la causa y el stack de un fallo del servidor nunca salen de él y se localizan en su log por el `request_id`

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout de dominio. Excepciones, igual que en controller-store: timeouts de `context` e infraestructura (el chequeo de salud de `/v1/status`, configurable con `api.Timeouts.Health`) y timeouts de red.
- `--controller` debe convivir con la dirección guardada en el bundle de credenciales: los SANs del certificado del servidor tienen que cubrirla, o el cliente fijado a la CA la rechazará.

## Referencias

- `docs/codefabric-spec.md` §18.1–18.2, §31.1–31.2, §34, §40

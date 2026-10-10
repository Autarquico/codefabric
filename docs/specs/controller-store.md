---
codename: codefabric
title: "Controller store: SQLite, transacciones, eventos e idempotencia"
type: task
status: shipped
priority: P0
size: M
issue: 4
prs: [29]
related_issues: [4]
sub_issues: []
depends_on: [domain-core]
supersedes: []
related_specs: [codefabric-spec, phase-0-skeleton]
created: 2026-10-10
updated: 2026-10-10
---

# Controller store: SQLite, transacciones, eventos e idempotencia

## Contexto / problema

Milestone de la [Fase 0 — Skeleton](phase-0-skeleton.md). Implementa §21.1–21.3, §27.4, §16.5, §20.2, §41 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- `internal/controller/store`: pool escritor (1 conexión) y lector, WAL, `synchronous=FULL`, `busy_timeout`, `_txlock=immediate`
- Migraciones con goose embebido; backup `VACUUM INTO` antes de migrar; rango 0001–0099
- `WithTx`: estado, evento y clave de idempotencia en una sola transacción
- Event store: `Sequence` global, `UNIQUE(worker_id, worker_epoch, worker_seq)`
- Idempotencia `(principal, operación, request_id)` con hash de payload y respuesta
- Audit y secuencias de short IDs
- `internal/controller/blob`: content-addressed, escritura atómica, nunca sobrescribe
- `internal/controller/eventbus`: fan-out tras commit
- Benchmark: 25 Workers × heartbeat 10 s + 100 Runs × 10 eventos/s sin `SQLITE_BUSY`

**No entra:**
- Tablas de dominio de fases posteriores

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Pragmas
- **Given** una base de datos nueva
- **When** se abre
- **Then** `journal_mode=wal` y `synchronous=FULL`

### Scenario: Backup previo a migrar
- **Given** una DB ya versionada (versión > 0) con una migración pendiente
- **When** arranca el Controller
- **Then** existe `controller.db.pre-<v>` antes de aplicarla (una DB nueva, en versión 0, no tiene nada que respaldar)

### Scenario: Atomicidad
- **Given** una mutación con `request_id` que hace panic dentro de la transacción
- **When** se reintenta
- **Then** no quedó ninguna fila, evento ni clave persistida y el reintento ejecuta limpio

### Scenario: Idempotencia
- **Given** una clave ya usada
- **When** se repite con el mismo payload / con otro payload
- **Then** misma respuesta sin eventos nuevos / `CF_IDEMPOTENCY_MISMATCH`

### Scenario: Deduplicación de eventos
- **Given** el mismo `(worker, epoch, seq)` dos veces
- **When** se hace `Append`
- **Then** queda un único evento

### Scenario: Blob con digest falso
- **Given** un upload cuyo digest no corresponde al contenido
- **When** se hace `Put`
- **Then** se rechaza

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout de dominio. Excepciones explícitas: `busy_timeout` de SQLite y los timeouts de `context` del llamador.

## Referencias

- `docs/codefabric-spec.md` §21.1–21.3, §27.4, §16.5, §20.2, §41

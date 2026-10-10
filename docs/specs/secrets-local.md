---
codename: codefabric
title: Secrets locales al Worker
type: task
status: active
priority: P0
size: M
issue: 23
prs: []
related_issues: [23]
sub_issues: []
depends_on:
  - scheduler-leases-reservations
  - worker-executor-tmux
  - harness-adapter-framework
supersedes: []
related_specs: [codefabric-spec, phase-2-harnesses-workflows]
created: 2026-10-10
updated: 2026-10-10
---

# Secrets locales al Worker

## Contexto / problema

Milestone de la [Fase 2 — Claude/Codex y Workflows](phase-2-harnesses-workflows.md). Implementa §8.10, §16.4, §16.7, §10.1, e2e #12 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- `internal/adapters/secrets/{file,keyring}`
- Entrega just-in-time en `runs/<id>/secrets/` (`0600`), borrada al terminar
- `internal/support/redact`: redacción en streaming tolerante a cortes entre chunks
- Metadata y allowlist en el Controller; migración `0203_secrets.sql`
- Credencial de origin como secret ref (`GIT_ASKPASS=cf worker git-askpass`)
- CLI: `cf worker secrets set|list|rm`, `cf project secrets allow|deny|list`

**No entra:**
- Backend central de secrets (Fase 10)

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Elegibilidad
- **Given** un secret disponible solo en B
- **When** se programa un Run que lo usa
- **Then** solo B es elegible y `explain` de A muestra `CF_SECRET_UNAVAILABLE`

### Scenario: Allowlist
- **Given** una ref fuera del allowlist
- **When** el Run intenta resolverla
- **Then** `CF_POLICY_DENIED` y entrada de audit

### Scenario: Redacción
- **Given** un Run que imprime el secret
- **When** se persiste el output
- **Then** contiene `[REDACTED:<ref>]` y el fichero JIT ya no existe

### Scenario: Solo metadata
- **Given** `cf worker secrets set`
- **When** se inspecciona el tráfico al Controller
- **Then** solo contiene metadata

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §8.10, §16.4, §16.7, §10.1, e2e #12

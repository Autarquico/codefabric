---
codename: codefabric
title: Git relay y publicación del resultado
type: task
status: active
priority: P0
size: L
issue: 12
prs: []
related_issues: [12]
sub_issues: []
depends_on: [projects-tasks-runs-api, worker-executor-tmux]
supersedes: []
related_specs: [codefabric-spec, phase-1-workers]
created: 2026-10-10
updated: 2026-10-12
---

# Git relay y publicación del resultado

## Contexto / problema

Milestone de la [Fase 1 — Workers y ejecución básica](phase-1-workers.md). Implementa §22.1, §22.4, §22.5, §22.6, §16.8 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng y devops-eng.

## Scope

**Entra:**
- `internal/controller/relay`: bare repo por Project con `denyNonFastForwards`/`denyDeletes`, `git http-backend` vía CGI, hook `pre-receive` que valida refs (§16.8)
- Autenticación git con token por lease vía `http.extraHeader` (nunca persistido)
- `internal/worker/gitops`: clone bare, `worktree add -b cf/<task>/<run>`, sellado con exclusiones, push con backoff
- `internal/app/runs/publication.go`: `publication: pending` en la misma transacción que el exit; `result_commit` al sellar; verificación con `rev-parse` antes de `published`
- Endpoint `POST /v1/runs/{ref}/publish` y CLI `cf run publish`

**No entra:**
- Checkpoints y snapshots en el relay (Fase 5)
- GC del relay (Fase 4)

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Exit atómico
- **Given** un harness que sale con 0
- **When** se registra el exit
- **Then** el Run queda `completed` + `publication: pending` en la misma transacción; un test garantiza que nunca existe `completed` + `none`

### Scenario: Sellado
- **Given** cambios sin commitear y ficheros untracked no ignorados
- **When** se sella
- **Then** existe el commit de sellado y `result_commit` está en `state.db` antes del primer reporte

### Scenario: Controller caído
- **Given** el Controller caído durante el push
- **When** vuelve
- **Then** el push se reintenta y el Run pasa a `published` con `RunPublished`

### Scenario: Refs protegidas
- **Given** un push a `main`, a la rama de otro Run o un force-push
- **When** llega al relay
- **Then** el hook lo rechaza

### Scenario: Fetch no autorizado
- **Given** un Worker sin Runs en el Project
- **When** hace fetch
- **Then** recibe 403

### Scenario: Sellado fallido
- **Given** disco lleno durante el sellado
- **When** falla el commit
- **Then** `publication: failed`, `RunPublicationFailed`, worktree intacto; `cf run publish` reintenta

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Decisiones de implementación

Las decisiones de #12 (relay vacío, transporte CGI propio, token por lease, sellado determinista con intención durable, publicación con estados locales y verificación en el Controller) están en el Apéndice B5 de `docs/codefabric-spec.md`.

## Referencias

- `docs/codefabric-spec.md` §22.1, §22.4, §22.5, §22.6, §16.8

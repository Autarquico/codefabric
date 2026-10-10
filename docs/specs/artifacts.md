---
codename: codefabric
title: "Artifacts"
type: task
status: draft
priority: P2
size: M
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [git-relay-publication, logs-output-storage]
supersedes: []
related_specs: [codefabric-spec, phase-7-experiments]
created: 2026-10-10
updated: 2026-10-10
---

# Artifacts

## Contexto / problema

Milestone de la [Fase 7 — Experiments](phase-7-experiments.md). Implementa §24, §16.8 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- Tabla `artifacts`; asociación autorizada por lease+epoch (también con Run terminal); artifacts automáticos de patch/commit; `cf artifact list|get`

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Sin lease
- **Given** un Worker sin lease del Run
- **When** asocia un blob
- **Then** se rechaza

### Scenario: Run terminal
- **Given** un Run terminal con epoch vigente
- **When** se asocia
- **Then** OK

### Scenario: Digest
- **Given** `cf artifact get`
- **When** se descarga
- **Then** el digest se verifica

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §24, §16.8

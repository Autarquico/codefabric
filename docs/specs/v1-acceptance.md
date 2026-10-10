---
codename: codefabric
title: "Aceptación v1.0"
type: task
status: draft
priority: P2
size: M
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [worker-provisioner, central-secrets, env-providers-devcontainer-mise-nix, notifications-integrations, opencode-pi-adapters, remote-artifact-storage, pty-relay-attach, scheduler-strategies-history, budgets-advanced, workflow-overlays-merged-offline, e2e-v0-5, e2e-v0-3, e2e-v0-3-tui, move-handoff, dirty-snapshots-auto-checkpoints, models-tui, provider-openrouter-google-harness-quota, e2e-v0-2]
supersedes: []
related_specs: [codefabric-spec, phase-10-elastic]
created: 2026-10-10
updated: 2026-10-10
---

# Aceptación v1.0

## Contexto / problema

Milestone de la [Fase 10 — Elastic workers e integrations](phase-10-elastic.md). Implementa §44 (1–18), §42.4 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: qa-tester.

## Scope

**Entra:**
- Matriz con cada criterio de §44 enlazado a tests existentes; los huecos se cierran aquí

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Matriz completa
- **Given** los 18 criterios de §44 y los 14 escenarios de §42.4
- **When** se revisa la matriz
- **Then** cada uno está enlazado a al menos un test en verde

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §44 (1–18), §42.4

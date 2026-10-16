---
codename: codefabric
title: Backend keyring de secrets
type: task
status: draft
priority: P0
size: M
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [secrets-local]
supersedes: []
related_specs: [codefabric-spec, phase-2-harnesses-workflows, secrets-local]
created: 2026-10-16
updated: 2026-10-16
---

# Backend keyring de secrets

## Contexto / problema

Milestone de la [Fase 2 — Claude/Codex y Workflows](phase-2-harnesses-workflows.md). Sale de [secrets-local](secrets-local.md) al recortar su alcance (decisión del usuario, 2026-10-16). Implementa el backend `keyring` de §8.10 y §16.4 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- `internal/adapters/secrets/keyring` (macOS Keychain y Secret Service en Linux) detrás del puerto de secrets-local
- `cf worker secrets set --backend keyring`; el catálogo local solo apunta a la entrada del llavero
- Fallos de llavero bloqueado o ausente como `CF_SECRET_UNAVAILABLE` con diagnóstico; consistencia catálogo–llavero y reparación de entradas huérfanas

**No entra:**
- Backend central (Fase 10)

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Keyring
- **Given** un secret guardado con `--backend keyring`
- **When** un Run autorizado lo usa
- **Then** recibe el valor y el catálogo local no lo contiene

### Scenario: Llavero bloqueado
- **Given** el llavero bloqueado o no disponible
- **When** un Run necesita el secret
- **Then** falla antes de arrancar con `CF_SECRET_UNAVAILABLE` y un diagnóstico, sin valores

### Scenario: Entrada huérfana
- **Given** una entrada del catálogo cuyo valor ya no está en el llavero
- **When** se ejecuta `cf worker secrets list`
- **Then** aparece marcada como rota y el heartbeat deja de anunciarla

## Notas técnicas / riesgos

- Los tests reales del llavero van gated (como `CF_REQUIRE_LAUNCHD`); en CI se usan dobles.

## Referencias

- `docs/codefabric-spec.md` §8.10, §16.4

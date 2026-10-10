---
codename: codefabric
title: "Fase 6 — Models/usage"
type: epic
status: draft
priority: P1
size: XL
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [phase-5-handoff]
supersedes: []
related_specs: [codefabric-spec]
created: 2026-10-10
updated: 2026-10-10
---

# Fase 6 — Models/usage

## Contexto / problema

Fase 6 del roadmap de `docs/codefabric-spec.md` (§43). Release objetivo: **v0.4**.

**Done de la fase:** Ver consumo global oficial y desglose interno sin confundir ambos.

## Scope

**Entra:** los milestones de la tabla. Cada uno tiene su propia spec con scenarios.

| Spec | Título | Size | Depende de |
|------|--------|------|------------|
| `usage-attribution` | Atribución de usage | M | `claude-code-adapter`, `codex-adapter`, `logs-output-storage` |
| `provider-anthropic-openai` | Providers Anthropic y OpenAI | M | `usage-attribution` |
| `provider-openrouter-google-harness-quota` | OpenRouter, Google y cuota vía harness | M | `provider-anthropic-openai` |
| `models-tui` | Vista Models del TUI | M | `usage-attribution`, `tui-views-actions` |

**No entra:** lo asignado a otras fases en §43.

## Criterios de aceptación

### Scenario: Done de la fase
- **Given** todos los milestones de la fase mergeados
- **When** se ejecuta su milestone `e2e-*` (si existe) y la verificación manual de la release
- **Then** se cumple: Ver consumo global oficial y desglose interno sin confundir ambos.

## Referencias

- `docs/codefabric-spec.md` §43, §43.1
- `docs/adr/0002-implementation-decisions.md`

---
codename: codefabric
title: "Backup y restore"
type: task
status: draft
priority: P1
size: M
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [retention-gc]
supersedes: []
related_specs: [codefabric-spec, phase-4-recovery]
created: 2026-10-10
updated: 2026-10-10
---

# Backup y restore

## Contexto / problema

Milestone de la [Fase 4 — Logs, recovery y reconciliation](phase-4-recovery.md). Implementa §28, §41, §44.14 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng y devops-eng.

## Scope

**Entra:**
- `cf backup`: `VACUUM INTO`, blobs, relay, config; CA solo con `--passphrase` (age); nunca secrets
- `cf restore`, `cf worker set-controller`, `cf connect --rebind`

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Consistencia
- **Given** escrituras en curso
- **When** se hace backup
- **Then** es consistente (`integrity_check`)

### Scenario: Restore en otro host
- **Given** un backup
- **When** se restaura en otro host
- **Then** mismos IDs y digests; los Workers reconectan cambiando solo la dirección

### Scenario: Sin secrets
- **Given** un Run que usó un secret
- **When** se inspecciona el backup
- **Then** el valor no aparece

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §28, §41, §44.14

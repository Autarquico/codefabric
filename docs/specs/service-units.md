---
codename: codefabric
title: Unidades de servicio systemd y launchd
type: task
status: shipped
priority: P0
size: S
issue: 15
prs: [39]
related_issues: [15]
sub_issues: []
depends_on: [worker-registry-heartbeat]
supersedes: []
related_specs: [codefabric-spec, phase-1-workers]
created: 2026-10-10
updated: 2026-10-15
---

# Unidades de servicio systemd y launchd

## Contexto / problema

Milestone de la [Fase 1 — Workers y ejecución básica](phase-1-workers.md). Implementa §4.3, §36.1, invariante 3 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: devops-eng.

## Scope

**Entra:**
- Plantillas embebidas: systemd del Controller, systemd `--user` del Worker con `KillMode=process`, launchd del Worker con `AbandonProcessGroup=true`
- CLI: `cf controller install-service`, `cf worker install-service|uninstall-service`

**No entra:**
- Distribución de binarios (Fase 10)

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: systemd
- **Given** la unit del Worker instalada y un Run activo
- **When** se ejecuta `systemctl --user restart cf-worker`
- **Then** la sesión tmux sobrevive

### Scenario: launchd
- **Given** el agente launchd instalado y un Run activo
- **When** se ejecuta `launchctl kickstart -k`
- **Then** la sesión tmux sobrevive

### Scenario: Idempotencia
- **Given** el servicio ya instalado
- **When** se instala de nuevo
- **Then** no cambia nada

### Scenario: Uninstall
- **Given** Runs activos
- **When** se desinstala
- **Then** no se matan Runs y se avisa

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §4.3, §36.1, invariante 3

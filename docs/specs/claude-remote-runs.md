---
codename: codefabric
title: "Los Runs remotos en Claude Code"
type: task
status: draft
priority: P2
size: M
issue: null
prs: []
related_issues: []
depends_on: [claude-handto]
supersedes: []
related_specs: [live-attach, claude-handto]
created: 2026-11-15
updated: 2026-11-15
---

# Los Runs remotos en Claude Code

## Contexto / problema

Tras `/handto`, la sesión sigue en un Worker, pero desde Claude Code ya no se ve: hay que salir a `cf attach`
o al TUI. Claude Code muestra las sesiones y agentes de la propia máquina; la idea es que los Runs de los
Workers aparezcan de forma parecida, marcados como remotos, y que se pueda entrar en ellos desde ahí.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- Ver desde Claude Code los Workers y sus Runs interactivos, marcados como remotos (Worker, estado, Task).
- Desde esa lista, entrar en un Run remoto (lo que hoy es `cf attach`) y volver.
- Instalado por `cf claude install`, como la skill `handto`.

**No entra:**
- Cambiar Claude Code: solo se usan sus puntos de extensión.
- Codex (después, si su CLI ofrece algo equivalente).

## Preguntas abiertas

- **Punto de extensión.** Sin verificar qué ofrece Claude Code para esto: una skill o comando que liste
  (mínimo), un panel o status line que se actualice, o integrarse con su lista de sesiones. Es lo primero
  del plan; si solo cabe una skill, el scope se reduce a listar y dar el comando de attach.
- Entrar desde Claude Code depende de `live-attach` si debe funcionar sin SSH.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Ver los remotos
- **Given** dos Workers online, uno con un Run interactivo de una sesión enviada con `/handto`
- **When** se abre la lista desde Claude Code
- **Then** aparecen los Workers con su estado y el Run marcado como remoto, con su Worker y su Task; los
  Workers offline se distinguen o no aparecen

### Scenario: Entrar en un remoto
- **Given** la lista con un Run remoto
- **When** se elige entrar
- **Then** se abre ese Run como con `cf attach`, y al salir se vuelve a Claude Code

### Scenario: Sin Controller
- **Given** `cf` sin Controller alcanzable
- **When** se abre la lista
- **Then** un error `CF_*` claro, sin colgar la sesión de Claude Code

## Referencias

- `docs/specs/claude-handto.md`, `docs/specs/live-attach.md`
- Issue #174 (listar Workers desde Claude Code)

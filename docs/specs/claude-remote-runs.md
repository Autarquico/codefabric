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
- Desde esa lista, el comando exacto para entrar en un Run remoto (`cf attach <task>`), que se abre en otra
  terminal.
- Un segmento de la status line de Claude Code con los Runs remotos vivos (opcional al instalar).
- Instalado por `cf claude install`, como la skill `handto`.

**No entra:**
- Cambiar Claude Code: solo se usan sus puntos de extensión.
- Codex (después, si su CLI ofrece algo equivalente).
- Entrar en el Run desde dentro de Claude Code: ninguna extensión documentada le cede la terminal a otro
  programa y vuelve (ver Decisiones). Un panel (mod) de Claude Code queda como exploración posterior.

## Decisiones

- Enmienda 2026-11-15. Comprobado en la documentación de Claude Code: no hay punto de extensión que añada
  filas a su lista de sesiones (`/resume`, `claude agents`); una skill o un comando `!` capturan la salida
  y no ceden la TTY; la status line ejecuta un comando local y se refresca (`refreshInterval`). Por eso el
  scope es una skill que lista y una línea de estado, y entrar se hace con `cf attach` en otra terminal.

## Preguntas abiertas

- `cf attach` sin SSH depende de `live-attach`.

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
- **Then** la skill da el comando exacto (`cf attach <task>`) y ese comando, en otra terminal, abre el Run;
  la sesión de Claude Code no se toca

### Scenario: Sin Controller
- **Given** `cf` sin Controller alcanzable
- **When** se abre la lista
- **Then** un error `CF_*` claro, sin colgar la sesión de Claude Code

## Referencias

- `docs/specs/claude-handto.md`, `docs/specs/live-attach.md`
- Issue #174 (listar Workers desde Claude Code)

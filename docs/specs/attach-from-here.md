---
codename: codefabric
title: Attach desde aquí (abrir una terminal con el attach)
type: feature
status: active
priority: P2
size: M
issue: 218
prs: []
related_issues: [218]
depends_on: [live-attach]
related_specs: [live-attach, claude-handto]
created: 2026-11-20
updated: 2026-11-20
---

# Attach desde aquí (abrir una terminal con el attach)

## Contexto / problema

El caso de uso: trabajo en un proyecto desde una sesión de Claude Code, mando una Task a otra máquina y, al rato,
quiero ver qué está haciendo sin salir de donde estoy.

Hoy no se puede. `cflk attach` exige una terminal en su entrada: sin TTY sale con `cflk attach needs a terminal on
its standard input` (`internal/cli/attach.go:103`). Un `!` de Claude Code y el servidor MCP corren sin TTY, y el MCP
no tiene herramienta para abrir una sesión: solo `runs.get` y `runs.logs` (`internal/mcp/tools.go:178-180`). Hay que
abrir otra terminal y teclear el attach a mano.

## Scope

**Entra:**
- `cflk attach` sin TTY abre una terminal nueva con el attach de verdad, si sabe cómo.
- Una herramienta MCP `runs.attach` que hace lo mismo, para que el modelo la abra cuando se le pide.
- La skill `codeflock` dice cuándo usarla.

**No entra:**
- Ver o escribir en la sesión sin terminal (pantalla en texto, enviar teclas): es otra cosa y queda fuera.
- Otras terminales que iTerm2, Terminal.app y tmux (Ghostty, WezTerm, kitty, terminales de Linux sin tmux): quedan
  como followup, con el mismo mecanismo.
- Cambiar el attach en sí (relay, ssh, tecla de salida, tamaño).

## Decisiones

- **Cuándo se abre una terminal.** Solo si la entrada no es una terminal y hay un lanzador conocido. Con TTY, el
  attach es el de ahora. Sin TTY y sin lanzador, el error de ahora, más el comando exacto para lanzarlo en una
  terminal. Un script sin TTY en una máquina sin lanzador no abre nada.
- **Lanzadores, en este orden:**
  1. Dentro de tmux (`TMUX` puesto): `tmux new-window` con el comando como argv, sin pasar por una shell.
  2. macOS con `TERM_PROGRAM=iTerm.app`: una ventana nueva de iTerm2.
  3. macOS con `TERM_PROGRAM=Apple_Terminal`: una ventana nueva de Terminal.app.
- **Qué corre en la ventana nueva:** la ruta absoluta de este mismo `cflk`, `attach`, el ID del Run ya resuelto (no
  el nombre de la Task) y los flags que importan (`--read-only`, `--ssh`, `--ssh-target`, `--controller`), con
  `CFLK_CONFIG_DIR` si estaba puesto. La ventana habla con el mismo Controller y el mismo Run que la orden original.
- **Antes de abrir nada se resuelve el attach.** Un Run que no existe, que no es interactivo o que ya acabó da su
  error de siempre y no abre ninguna ventana (`resolveAttach` ya va antes del chequeo de TTY).
- **Nada se interpreta.** El comando no se mete en el texto de un AppleScript: va a `osascript` como argumento
  (`on run argv`). Cada argumento se cita para la shell que lo ejecuta. tmux recibe argv. Ni el nombre que da el
  usuario ni lo que devuelve el servidor llegan a una shell sin citar.
- **Salida de la orden original:** dice dónde se abrió (`Opened run-49 in a new iTerm2 window`) y sale con 0. Con
  `--json`, `{"opened": "iterm2"|"terminal"|"tmux", "run": "<id>"}`.
- **`runs.attach` en el MCP:** recibe `ref`, `project` y `read_only`, y devuelve lo mismo que `--json`. Está en los
  perfiles `execute` y `modify`. En `read` también está, pero siempre de solo lectura: quien teclea es la persona,
  y un perfil de solo lectura no le abre una sesión que escribe. Sin lanzador, el error lleva el comando para que el
  modelo se lo pase a la persona.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`. Los lanzadores se prueban con un ejecutor falso que
registra el argv; un test de integración con tmux real cubre el de tmux.

### Scenario: Sin terminal, dentro de tmux
- **Given** un Run interactivo en marcha y `cflk attach <task>` lanzado sin TTY con `TMUX` puesto
- **When** termina la orden
- **Then** hay una ventana nueva de tmux con `cflk attach <run-id>`, y la orden dice dónde se abrió y sale con 0

### Scenario: Sin terminal, en iTerm2 o Terminal.app
- **Given** macOS, sin TTY, con `TERM_PROGRAM` de iTerm2 (o de Terminal.app)
- **When** se lanza `cflk attach <run>`
- **Then** se abre una ventana nueva de esa terminal con el attach, por `osascript` con el comando como argumento

### Scenario: Con terminal, nada cambia
- **Given** `cflk attach <run>` lanzado en una terminal
- **When** se ejecuta
- **Then** es el attach de ahora, en esa misma terminal, y no se abre ninguna ventana

### Scenario: Sin lanzador
- **Given** sin TTY, fuera de tmux y sin una terminal conocida
- **When** se lanza `cflk attach <run>`
- **Then** sale con `CF_INVALID_REQUEST` como ahora, y el mensaje trae el comando exacto para lanzarlo en una terminal

### Scenario: El Run no se puede abrir
- **Given** un Run que no existe, que no es interactivo o que ya acabó
- **When** se lanza sin TTY con un lanzador disponible
- **Then** sale con su error de siempre y no se abre ninguna ventana

### Scenario: La ventana es el mismo attach
- **Given** `cflk attach <task> --read-only --controller h:p` sin TTY
- **When** se abre la ventana
- **Then** corre la misma ruta de `cflk`, con el ID del Run resuelto, `--read-only` y `--controller h:p`

### Scenario: Nada se interpreta
- **Given** un nombre de Task o un ID con comillas, `;`, `$()` o saltos de línea
- **When** se abre la ventana
- **Then** el argv que llega al lanzador lo lleva como texto literal, y no se ejecuta nada más

### Scenario: Desde el MCP
- **Given** el servidor MCP con perfil `execute`, en una sesión de iTerm2
- **When** el modelo llama a `runs.attach` con el nombre de una Task en marcha
- **Then** se abre la ventana con el attach, y la herramienta devuelve `{opened, run}`

### Scenario: MCP de solo lectura
- **Given** el servidor MCP con perfil `read`
- **When** el modelo llama a `runs.attach` con `read_only: false`
- **Then** la ventana se abre con `--read-only`

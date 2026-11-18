---
codename: codefabric
title: Arreglos de la batería de live-attach y Claude Code (beta.5)
type: task
status: active
priority: P1
size: L
issue: 201
prs: [204]
related_issues: [201]
depends_on: [live-attach, claude-code-adapter]
related_specs:
  - codefabric-spec
  - live-attach
  - claude-code-adapter
  - logs-output-storage
created: 2026-11-18
updated: 2026-11-18
---

# Arreglos de la batería de live-attach y Claude Code (beta.5)

## Contexto / problema

La batería de pruebas sobre la flota real (Controller y Workers en `v0.4.0-beta.5`, 2026-11-17) dejó nueve
hallazgos, recogidos con su reproducción en la issue #201. El que limita el uso real es que la respuesta
de un Run headless no se puede leer; dos más hacen fácil guardar mal el token de Claude; el resto son
detalles del doctor, del TUI y de `cf attach`.

Esta spec solo da forma de scenarios a la issue: el detalle de cada hallazgo está allí.

## Scope

**Entra:** los nueve hallazgos de #201.

**No entra:** los cortes de red de mac-mini (no son de cf).

## Decisiones

- La respuesta de un Run headless es un dato del Run (`answer`), que viaja por el informe de salida como
  ya lo hace el modelo; `cf logs --run` la muestra al final solo en el modo humano. Un Worker solo la manda
  a un Controller que anuncia que la acepta.
- `claude/oauth-token` con un valor que no tiene formato de token se rechaza; `--force` lo guarda igual.
- Un enlace simbólico directo bajo `skills/`, `commands/` o `agents/` se sigue un nivel, si su destino es
  un directorio del usuario o de root que nadie más puede escribir; dentro se siguen rechazando enlaces.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Respuesta de un Run headless
- **Given** un Run headless de `claude-code` o `codex` que termina con una respuesta final
- **When** se pide `cf run show`, `cf logs --run` o `runs.get` del MCP
- **Then** se ve la respuesta, acotada y redactada como su salida

### Scenario: Versiones mezcladas
- **Given** un Worker beta.6 y un Controller que no anuncia que acepta la respuesta
- **When** termina un Run headless
- **Then** el informe de salida llega sin la respuesta y se acepta

### Scenario: Token pegado en dos líneas
- **Given** `cf worker secrets set claude/oauth-token` en una terminal
- **When** se pega dos veces un token partido en dos líneas
- **Then** se guarda el token entero

### Scenario: Valor que no es un token
- **Given** `cf worker secrets set claude/oauth-token`
- **When** el valor no tiene la forma `sk-ant-oat…`
- **Then** no se guarda nada y el error dice qué valor se espera; con `--force` se guarda, y el doctor
  avisa de que no parece un token

### Scenario: Skill enlazada
- **Given** `~/.claude/skills/<name>` es un enlace a un directorio fuera de `~/.claude`
- **When** se hace una snapshot de la configuración (doctor, adopt, apply global)
- **Then** se copian sus ficheros como ficheros normales; un enlace dentro de él, o a un destino que otro
  usuario puede escribir, se rechaza

### Scenario: Salir de cf attach
- **Given** un `cf attach` en una terminal
- **When** se sale con la tecla de salir
- **Then** el aviso de entrada sigue visible y lo siguiente se escribe debajo

### Scenario: Rejilla de tres Runs
- **Given** la rejilla con tres Runs vivos que llenan su pantalla
- **When** se pinta a 160x45
- **Then** las dos filas tienen la misma altura

### Scenario: Shell denegada
- **Given** la shell denegada por la policy del Project o la del Worker
- **When** se ve el error en el TUI
- **Then** la línea "Try" dice cómo habilitar esa policy

### Scenario: Actividad con nombres
- **Given** eventos de lease y de Worker
- **When** se ven en la Actividad del TUI
- **Then** muestran el nombre de la Task y del Worker, no un ID recortado

### Scenario: Tecla de respaldo al entrar
- **Given** `attach.detach_key` distinto de `ctrl+]`
- **When** se pulsa `ctrl+]` en cuanto se entra en un Run
- **Then** se sale del Run

## Referencias

- Issue #201
- `docs/specs/live-attach.md`, `docs/specs/claude-code-adapter.md`, `docs/specs/logs-output-storage.md`

---
codename: codefabric
title: Arreglos de la beta.7 (attach de solo lectura y Run headless que no sale)
type: task
status: active
priority: P1
size: M
issue: 208
prs: []
related_issues: [208, 203]
depends_on: [live-attach, claude-code-adapter]
related_specs:
  - live-attach
  - claude-code-adapter
created: 2026-11-19
updated: 2026-11-19
---

# Arreglos de la beta.7 (attach de solo lectura y Run headless que no sale)

## Contexto / problema

Dos fallos vistos en la flota, con su reproducción en las issues:
- #208: `cf attach --read-only` a 160x45 ve un recorte de 80x24 de la sesión. Ni el cliente, ni el Controller,
  ni el Worker pasan el tamaño de un cliente que solo mira.
- #203: un Run headless de `claude-code` puede seguir vivo e inactivo durante horas tras su último `result`, con la
  entrada ya cerrada. El Run se queda en `running`.

Esta spec solo da forma de scenarios a las dos issues: el detalle está allí.

## Scope

**Entra:** #208 y #203.

**No entra:** cambiar el argv de tmux del attach (`-r` ya es `read-only,ignore-size` desde tmux 3.2), ni parar
por inactividad un harness que no sea claude-code.

## Decisiones

- El que mira manda su tamaño, el Controller lo deja pasar y el Worker lo aplica a su pty, solo si el tmux del
  Worker es 3.2 o posterior. Lo que teclea sigue sin llegar.
- Un Run headless de claude-code se da por terminado, y se para, solo si su último evento es un `result` de éxito,
  `input.eof` está escrito, no hay preguntas abiertas, y ni sus eventos ni su stderr han crecido en 120 s. Lo
  decide el executor del Worker. El exit real se conserva.
- El Worker marca esa parada con `end_reason: idle_after_result`, solo si el Controller anuncia que lo acepta. El
  Controller la liquida como `completed`, con la respuesta del `result`.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: El que mira ve la sesión entera
- **Given** un Run interactivo con un cliente que escribe a 160x45, en un Worker con tmux 3.2 o posterior
- **When** se abre `cf attach --read-only` a 160x45
- **Then** ve la ventana entera, y lo que teclea no llega al Run

### Scenario: El tamaño del que mira no cambia la ventana
- **Given** un attach de solo lectura y otro que escribe
- **When** el que mira empieza más pequeño, o redimensiona su terminal
- **Then** la ventana del Run sigue con el tamaño del que escribe

### Scenario: Attach de solo lectura con versiones mezcladas
- **Given** un Controller o un Worker beta.6 en el camino
- **When** se abre `cf attach --read-only`
- **Then** funciona como en la beta.6

### Scenario: Run headless que no sale tras acabar
- **Given** un Run headless de claude-code cuyo último evento es un `result` de éxito, con la entrada cerrada
- **When** pasan 120 s sin eventos ni stderr nuevos y el harness sigue vivo
- **Then** el Worker lo para y el Run queda `completed`, con la respuesta de ese `result`

### Scenario: Lo que no se para por inactividad
- **Given** un Run headless de claude-code tras un `result`
- **When** el `result` es de error, llega una pregunta, o sigue escribiendo eventos o stderr
- **Then** no se para por inactividad

### Scenario: Parada por inactividad con un Controller beta.6
- **Given** un Controller que no anuncia que acepta `idle_after_result`
- **When** el Worker para un Run por inactividad
- **Then** el informe se acepta y el Run se liquida como hoy

## Referencias

- Issues #208 y #203
- `docs/specs/live-attach.md`, `docs/specs/claude-code-adapter.md`

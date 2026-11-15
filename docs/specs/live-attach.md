---
codename: codefabric
title: Entrar en un Run o un Worker desde el TUI
type: task
status: active
priority: P1
size: L
issue: 179
prs: [191]
related_issues: [179]
depends_on: [observe-attach-events, tui-views-actions]
supersedes: [pty-relay-attach]
related_specs: [codefabric-spec, claude-remote-runs]
created: 2026-11-15
updated: 2026-11-15
---

# Entrar en un Run o un Worker desde el TUI

## Contexto / problema

Hoy, ver lo que hace un Run obliga a salir del TUI: la acción attach lo suspende y lanza `cf attach`, que
entra por SSH al Worker y hace `tmux attach` a la sesión del Run; se vuelve con `Ctrl-b d`. Tres cosas lo
alejan de "entrar y salir de una máquina como si estuvieras en ella":

- No se ve nada sin entrar: no hay una vista en directo dentro del TUI.
- La salida es la de tmux: dentro de un tmux propio, `Ctrl-b d` sale del de fuera.
- Cada cliente necesita SSH y claves de cada Worker (§5.4 lo deja para "posterior"; en el lab de la Fase 5,
  `cf attach` a mac-mini preguntó por la clave de `mac-mini.local` aunque ya la conocía con otro nombre).

Esta spec cubre las tres y añade abrir una shell en el Worker. Absorbe `pty-relay-attach` (§5.4: relay de PTY
por el daemon del Worker a través del Controller).

Owner previsto en el bucle Develop: backend-eng (relay, Worker, API) y frontend-eng (TUI).

## Scope

**Entra:**
- **Relay de PTY por el Controller.** El cliente abre un stream con el Controller; el Worker, que ya mantiene
  su conexión saliente, abre el otro extremo; el Controller los empalma. Ningún cliente necesita SSH al
  Worker. `cf attach` pasa a usarlo por defecto; el SSH queda como alternativa explícita mientras conviva.
- **Vista en directo en el TUI.** En las vistas Tasks y Runs, un panel muestra la pantalla actual del Run
  seleccionado (solo lectura) y se actualiza sin salir del TUI. Se pueden tener varios a la vez (rejilla).
- **Entrar y salir.** Una tecla del TUI pasa el Run del panel a pantalla completa e interactiva; una tecla
  propia de `cf` (no la de tmux, configurable) devuelve al mismo sitio del TUI, también si el TUI corre dentro
  de tmux. Salir nunca termina el Run.
- **Escritores múltiples.** Todos los conectados pueden escribir, como en un tmux compartido; el TUI muestra
  quién más está dentro de ese Run. `--read-only` se mantiene.
- **Shell en el Worker.** Abrir una shell en el worktree del Run (`cf shell <run>` y acción del TUI), para
  mirar ficheros y git junto al agente, por el mismo relay. Opcional y desactivada por defecto: la habilitan
  a la vez la policy del Project y la del Worker.
- Auditoría: cada entrada interactiva y cada shell queda en eventos (quién, qué Run o Worker, cuándo, cuánto).

**No entra:**
- Shell en un Worker sin Run (la máquina en sí, fuera de un worktree): se decide tras ver el uso de la de Run.
- Grabación y reproducción de sesiones.
- Clientes web.

## Decisiones

- El transporte es el Controller y no SSH: es el modelo de §5.4 (todo inicia hacia el Controller) y quita
  la gestión de claves por cliente y Worker.
- La shell sobre un Worker cambia su modelo de seguridad (pasa a dar shells a quien la policy permita):
  por eso está desactivada por defecto y auditada.
- Quién puede (enmienda 2026-11-15, tras la critique del plan): en v0.x todo cliente enrolado es una máquina
  del owner. Ver y escribir en un Run: cualquier cliente. Shell: solo si la habilitan a la vez el Project
  (`shell`) y el Worker (`allow_shell`, local, comprobado justo antes de lanzarla). Una identidad de Worker
  nunca abre un attach. La shell tiene la autoridad del usuario del SO del Worker: el worktree no aísla.
- Cada entrada es una cita de un solo uso, atada al cliente, al Worker, al Run, al epoch de su lease, al modo
  (ver, escribir, shell) y a una caducidad. Un stream abierto se corta al revocar la identidad, al cambiar la
  lease o al apagar el Controller.
- SSH solo con `--ssh` explícito: sin vuelta automática a SSH si el Worker no ofrece el relay.
- La salida es una tecla de `cf` (por defecto `ctrl+]`, configurable); el tmux de fuera tiene que dejarla
  pasar, y la guía lo dice.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Attach sin SSH
- **Given** un cliente conectado al Controller y sin SSH al Worker, y un Run interactivo en marcha
- **When** se hace `cf attach <task>`
- **Then** se abre la sesión del Run a través del Controller; cerrar el attach no termina el Run

### Scenario: Vista en directo
- **Given** el TUI en la vista Tasks con un Run interactivo en marcha seleccionado
- **When** el agente escribe en su terminal
- **Then** el panel lo muestra en menos de 1 s sin salir del TUI, en solo lectura

### Scenario: Varios a la vez
- **Given** tres Runs interactivos en marcha en Workers distintos
- **When** se abre la rejilla
- **Then** se ven los tres en directo a la vez

### Scenario: Entrar y salir
- **Given** el panel de un Run en el TUI, con el TUI corriendo dentro de tmux
- **When** se entra con la tecla de entrar, se teclea y se pulsa la tecla de salir de `cf`
- **Then** lo tecleado llega al Run, se vuelve al mismo sitio del TUI y el Run sigue `running`; el tmux de
  fuera no se toca

### Scenario: Dos escritores
- **Given** dos clientes dentro del mismo Run
- **When** los dos teclean
- **Then** el Run recibe lo de ambos, cada uno ve la pantalla del otro y el TUI de cada uno lista al otro

### Scenario: Solo lectura
- **Given** un cliente con `--read-only`
- **When** teclea
- **Then** no llega nada al Run

### Scenario: Shell desactivada
- **Given** un Project sin la shell habilitada
- **When** se pide `cf shell <run>`
- **Then** error `CF_POLICY_DENIED` que nombra la policy, y ninguna shell abierta

### Scenario: Shell en el worktree
- **Given** la shell habilitada en el Project y en el Worker, y un Run en marcha
- **When** se pide `cf shell <run>`
- **Then** se abre una shell en el worktree de ese Run en su Worker, a través del Controller, y queda un
  evento de auditoría con el usuario y la duración

### Scenario: Shell vetada por el Worker
- **Given** un Project con la shell habilitada y un Worker sin `allow_shell`
- **When** se pide `cf shell <run>`
- **Then** error `CF_POLICY_DENIED` que nombra la policy del Worker, y ninguna shell abierta; si el Worker la
  retira después de pedirla y antes de lanzarla, tampoco se abre

### Scenario: Cita de un solo uso
- **Given** una entrada concedida a un cliente para un Run
- **When** otro Worker la reclama, se reclama dos veces, se reclama caducada o con un epoch de lease viejo
- **Then** se rechaza y no se abre nada; una identidad de Worker que pide entrar en un Run recibe
  `CF_POLICY_DENIED`

### Scenario: Stream que deja de valer
- **Given** un cliente dentro de un Run
- **When** se revoca su identidad, o la lease del Run cambia de epoch
- **Then** el stream se corta en ambos extremos con un error `CF_*` y queda el evento de cierre

### Scenario: Controller que se apaga
- **Given** un cliente dentro de un Run
- **When** el Controller se apaga
- **Then** el stream se corta en ambos extremos, el cliente vuelve al TUI con un error `CF_*` y el Run sigue
  `running` en su Worker

### Scenario: Sin relay en el Worker
- **Given** un Worker que no anuncia el relay
- **When** se hace `cf attach <task>` sin `--ssh`
- **Then** un error `CF_*` que dice que ese Worker solo admite `--ssh`, sin intentar SSH; con `--ssh` entra
  como hoy

### Scenario: Worker que cae
- **Given** un cliente dentro de un Run
- **When** el Worker pierde la conexión
- **Then** el cliente ve el corte con el código `CF_WORKER_OFFLINE` y vuelve al TUI; al volver el Worker se
  puede entrar de nuevo

## Notas técnicas / riesgos

- Latencia y ancho de banda: el relay añade un salto. Medir eco de tecla y coste por Run visto en rejilla;
  la vista en directo puede ir a menor frecuencia que el modo interactivo.
- Backpressure por cliente: uno lento no debe frenar al Run ni a los demás.
- El Worker ya captura el pane (tmux); la vista puede salir de ahí o del stream del PTY: decidir en el plan.
- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).

## Referencias

- `docs/codefabric-spec.md` §5.4, §6
- `docs/specs/observe-attach-events.md`, `docs/specs/tui-views-actions.md`, `docs/specs/archive/2026-11-15-pty-relay-attach.md`

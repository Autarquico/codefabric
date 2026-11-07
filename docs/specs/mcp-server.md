---
codename: codefabric
title: "Servidor MCP"
type: task
status: draft
priority: P1
size: L
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [client-subscriptions]
supersedes: []
related_specs: [codefabric-spec, phase-8-mcp-plans, plans-dag, confirmations-sessions-permissions]
created: 2026-10-10
updated: 2026-11-07
---

# Servidor MCP y skills para Claude Code y Codex

## Contexto / problema

§17 pide que un modelo controle CodeFabric desde una sesión MCP sin shell arbitraria. Esta spec lo adelanta desde la [Fase 8](phase-8-mcp-plans.md) a la v0.2.0, decisión del 2026-11-07. El objetivo es usar `cf` desde las CLIs de Claude Code y Codex sin salir de ellas.

Se recorta a lo que ya existe en el Core. Plans, fork/compare/promote, budgets, artifacts, models y confirmaciones entran cuando existan sus specs: [plans-dag](plans-dag.md), [confirmations-sessions-permissions](confirmations-sessions-permissions.md), etc. Cada tool nueva se añade aquí con el mismo nombre de §17.2.

Decisiones:
- **Transporte:** `cf mcp serve` por stdio, con el go-sdk oficial. Habla con el Controller a través del Client SDK, usando la identidad mTLS del cliente ya enrolado (`cf connect`). El MCP por HTTP servido por el Controller queda como followup.
- **Tools:** solo las operaciones que ya existen.
- **Permisos:** perfiles read, execute y modify (§16.3). Lo que §16.3 marca como `require_confirmation` no se expone hasta que existan las confirmaciones.

Owner previsto en el bucle Develop: backend-eng (mcp, SDK, API) y qa-tester.

## Scope

**Entra:**

- **`cf mcp serve [--profile read|execute|modify]`.** Perfil por defecto: `execute`.
  - Servidor MCP por stdio en `internal/mcp`, que usa solo `internal/client` y `internal/api/v1` (ADR 0001).
  - Se cablea desde `cmd/cf`, o desde `internal/cli` si se amplía su allow-list (se decide en el plan).
- **Tools, mapeadas 1:1 al Client SDK con los nombres de §17.2.** Hay tres alias:
  - `runs.logs` es la salida del Run.
  - `workflows.diff` es un apply en dry-run.
  - `inbox.list` y `search` añaden el Inbox y la búsqueda de §18.2.

  | Perfil | Tools |
  |---|---|
  | read | `projects.list`, `projects.get`, `tasks.list`, `tasks.get`, `runs.list`, `runs.get`, `runs.logs`, `workers.list`, `workers.get`, `workflows.list`, `workflows.get`, `workflows.diff`, `inbox.list`, `search`, `logs.query`, `events.query`, `events.wait` |
  | execute | read + `tasks.create`, `tasks.respond` |
  | modify | execute + `tasks.cancel`, `workers.drain`, `workers.undrain` |

- **Lo que no tiene un perfil no se lista.** Llamar a una tool que no se ha listado da error.
- **No hay tools de shell ni `secrets.*`** en ningún perfil.
- **`request_id` es obligatorio en toda tool mutante.**
  - El SDK se amplía donde hoy no lo pasa: `CancelTask`, y drain/undrain de Workers, porque `taskAction` envía un `ActionRequest` vacío.
  - Repetir una tool con el mismo `request_id` no produce un segundo efecto (§27.4).
- **`events.wait {types, filter{project,task,run,worker}, after_cursor, timeout}`** (§17.3).
  - Se construye sobre el stream de events, sin polling. Bloquea hasta el primer event que cumple el filtro o hasta el timeout (por defecto 5 min, máximo 10 min) y devuelve `{events, cursor}`.
  - Sin `after_cursor`, devuelve el cursor actual. Un cursor caducado da `CF_CURSOR_EXPIRED`.
  - El reloj es inyectable.
- **Auditoría de sesión MCP (§15, §16).**
  - Cada `cf mcp serve` abre una sesión con un ID propio.
  - Las mutaciones se registran con actor `{kind: mcp, id: <principal del cliente>, session: <id>}`. Así se usa `ActorMCP`, que hoy no se asigna nunca.
  - El Controller solo acepta el tipo `mcp` de un principal humano y nunca lo eleva. Un Worker no puede declararse `mcp`.
- **Skills empaquetadas en el binario**, una para Claude Code (`SKILL.md`) y otra para Codex (formato de skills de Codex). Explican:
  - cuándo usar CodeFabric;
  - el flujo: `tasks.create`, luego `events.wait` hasta `RunCompleted`, `RunFailed` o `TaskNeedsInput`, luego `inbox.list` y `tasks.respond`;
  - que no se hace polling;
  - que hay que pasar siempre `request_id`;
  - que las limitaciones de perfil son deliberadas.
- **`cf mcp install --claude|--codex [--scope user|project] [--profile …] [--dry-run]`.**
  - Registra el servidor con la CLI de cada herramienta: `claude mcp add --scope … cf -- cf mcp serve --profile …` o `codex mcp add cf -- cf mcp serve --profile …`. Escribe la skill en el directorio de skills de cada herramienta.
  - Es idempotente. Avisa si la CLI no está instalada.
  - `cf mcp uninstall` deshace ambas cosas.
- **Docs:** `docs/guide/mcp.md`, una sección del CLI en `cli.md`, y en la landing solo lo que esté verificado.

**No entra:**

- Plans, `runs.fork`, `runs.compare`, `runs.promote`, `runs.start` y `runs.stop`.
  - El SDK no tiene `StopRun`; `tasks.cancel` cubre parar.
- `budgets.*`, `artifacts.*`, `models.*`, `confirmations.*`, `workers.capacity`.
  - La capacidad va dentro de `workers.get`.
- MCP por HTTP desde el Controller y OAuth.
- Notificaciones push del servidor MCP (`notifications/*`). `events.wait` es la primitiva.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Master
- **Given** un cluster de test con un Worker y un cliente MCP del go-sdk conectado a `cf mcp serve` (transporte en memoria o stdio)
- **When** llama a `tasks.create` y luego a `events.wait{types:[RunCompleted], filter:{task}}`
- **Then** recibe el event, y `runs.get` y `runs.logs` devuelven el resultado y la salida

### Scenario: Needs-input desde el modelo
- **Given** una Task con `--permissions ask` que espera un permiso
- **When** el modelo recibe `TaskNeedsInput` por `events.wait`, lee `inbox.list` y llama a `tasks.respond{allow}` con `request_id`
- **Then** el Run continúa y el event de respuesta lleva actor `mcp` con el ID de sesión

### Scenario: Cursor caducado
- **Given** un cursor anterior a la retención
- **When** se llama a `events.wait{after_cursor}`
- **Then** devuelve `CF_CURSOR_EXPIRED`, y una llamada sin cursor devuelve el cursor actual

### Scenario: Timeout de espera
- **Given** un filtro que no casa con ningún event
- **When** se llama a `events.wait{timeout: 30s}` con el reloj inyectado
- **Then** al vencer devuelve `events: []` y un cursor reanudable, y un timeout mayor de 10 min se rechaza o se recorta a 10 min, según lo que fije el plan

### Scenario: Perfiles
- **Given** `cf mcp serve --profile read`
- **When** se listan las tools y se llama a `tasks.create`
- **Then** no aparece ninguna tool mutante y la llamada da error. Con `execute` aparecen `tasks.create` y `tasks.respond` pero no `tasks.cancel`, y con `modify` aparecen las tres

### Scenario: Sin shell
- **Given** el listado de tools con cualquier perfil
- **When** se inspecciona
- **Then** no hay ninguna tool de shell ni `secrets.*`

### Scenario: request_id
- **Given** una tool mutante
- **When** se llama sin `request_id`, y después dos veces con el mismo
- **Then** la primera llamada da un error de validación, y la repetida tiene un único efecto y la misma respuesta

### Scenario: Instalación en Claude Code y Codex
- **Given** binarios `claude` y `codex` falsos en el PATH que registran sus argumentos, y un HOME temporal
- **When** se ejecuta `cf mcp install --claude --scope user` y luego `--codex`, dos veces cada uno
- **Then** se invoca `claude mcp add … cf -- cf mcp serve --profile execute` y `codex mcp add cf -- …`, las skills quedan en sus directorios con su contenido, la segunda ejecución no duplica nada y `cf mcp uninstall` lo deshace todo

## Notas técnicas / riesgos

- **Capas y decisiones:** respetar ADR 0001. `internal/mcp` no importa app, controller, worker ni adapters, y `.golangci.yml` ya lo cubre. Respetar también ADR 0002 (D1–D7).
- **Dependencia nueva:** `github.com/modelcontextprotocol/go-sdk`, en una versión exacta. El `mcpbridge` del Worker está hecho a mano y no se migra aquí.
- **Atomicidad:** transacción única para estado, event e idempotencia (§27.4). Reloj inyectable en todo timeout.
- **Actor `mcp`:** el cliente declara la sesión en una cabecera (por ejemplo `CF-MCP-Session`). El servidor la acepta solo de principals humanos, valida su formato y la lleva al actor del event. Una sesión no da permisos: los perfiles los aplica `cf mcp serve` y son una restricción local para el modelo, no una frontera de seguridad frente al dueño de la máquina. Esto se dice tal cual en la doc.
- **Skills:** su contenido se versiona con el binario. `cf mcp install` las reescribe al actualizar, y no toca otras skills del usuario.
- **Verificación real:** tests opt-in con Claude Code y Codex reales registrando el MCP, como los de los adapters. Por defecto, cliente go-sdk contra el servidor.

## Referencias

- `docs/codefabric-spec.md`: §16.3 (permisos MCP), §17 (MCP), §18.2 (CLI), §27.4 (idempotencia).

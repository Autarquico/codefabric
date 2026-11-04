---
codename: codefabric
title: Credencial de origin vía GIT_ASKPASS
type: task
status: draft
priority: P0
size: M
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [secrets-local]
supersedes: []
related_specs: [codefabric-spec, phase-2-harnesses-workflows, secrets-local]
created: 2026-10-16
updated: 2026-10-16
---

# Credencial de origin vía GIT_ASKPASS

## Contexto / problema

Milestone de la [Fase 2 — Claude/Codex y Workflows](phase-2-harnesses-workflows.md). Sale de [secrets-local](secrets-local.md) al recortar su alcance (decisión del usuario, 2026-10-16). Implementa la credencial de origin como secret ref (§16.7 de `docs/codefabric-spec.md`, ADR 0002 D1/D3).

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- `origin_secret` del Project como ref autorizada por el grant de secrets-local
- `GIT_ASKPASS` con un script ejecutable de ruta absoluta que llama a `cf worker git-askpass`; usuario como metadata y token como valor; solo HTTPS y solo el destino de origin autorizado; redirecciones desactivadas o validadas; `credential.helper=` vacío
- El valor sale solo por stdout hacia git; la salida de git se redacta antes de cualquier buffer acotado, log o reporte
- Retención: la credencial deja de estar disponible para el harness tras el fetch, pero sus agujas se conservan para la redacción y el guard hasta cerrar captura y publicación

**No entra:**
- Claves SSH (followup `origin-auth-ssh`)

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Repo privado
- **Given** un Project con `origin_secret` permitido y el secret en el Worker
- **When** se prepara un Run
- **Then** el fetch de origin funciona y ningún helper de git guarda la credencial

### Scenario: Destino no autorizado
- **Given** una redirección o un remote distinto del origin del Project
- **When** git pide credenciales
- **Then** askpass no responde y el Run falla sin filtrar el valor

### Scenario: Error de fetch
- **Given** un fetch que imprime la credencial en su error
- **When** se registra el fallo
- **Then** logs, reportes y output muestran `[REDACTED:<ref>]`

## Notas técnicas / riesgos

- Probar con git real en las versiones soportadas y con rutas con espacios.
- Usuario: `cf worker secrets set <ref> --user <u>`; si falta, `x-access-token` (decisión del usuario, 2026-11-04).
- El fetch autenticado corre con config de git aislada (`GIT_CONFIG_GLOBAL=/dev/null`, `GIT_CONFIG_NOSYSTEM=1`); un proxy solo se respeta vía el entorno del daemon (decisión del usuario, 2026-11-04).
- El destino autorizado es esquema + host:puerto + ruta del repo (`credential.useHttpPath=true`); otro repo del mismo host no recibe la credencial.
- Un Worker sin la capacidad `origin-askpass:v1` no es elegible para un Run cuyo Project tiene `origin_secret`.
- Límite aceptado (decisión del usuario, 2026-11-04): el harness corre con el mismo usuario del SO, así que la revocación es de acceso vía `cf`, no aislamiento del SO; las agujas retenidas en el directorio del Run son legibles por ese usuario. Un broker queda como followup.

## Referencias

- `docs/codefabric-spec.md` §16.7; ADR 0002 D1, D3

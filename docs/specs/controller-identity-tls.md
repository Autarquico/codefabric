---
codename: codefabric
title: Identidad del Controller, TLS y tokens de cliente
type: task
status: active
priority: P0
size: M
issue: 5
prs: []
related_issues: [5]
sub_issues: []
depends_on: [controller-store]
supersedes: []
related_specs: [codefabric-spec, phase-0-skeleton]
created: 2026-10-10
updated: 2026-10-10
---

# Identidad del Controller, TLS y tokens de cliente

## Contexto / problema

Milestone de la [Fase 0 — Skeleton](phase-0-skeleton.md). Implementa §16.6, §35, §36.1 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng (revisa devops-eng).

## Scope

**Entra:**
- Spike inicial: CA y hojas X.509 en ECDSA P-256 con git/libcurl de macOS y Debian (ADR 0002, D2)
- `internal/controller/pki`: CA, certificados, tokens de un solo uso (hash, TTL, kind), invitaciones `cf://host:port/join?token=…#sha256:<fp>`
- mTLS con comprobación de revocación contra `identities`
- `internal/config`: paths XDG, config de Controller y cliente
- `internal/client/credentials`: keychain o fichero `0600`, CA fijada
- Endpoints `/v1/join` (canje atómico), `/v1/identity/renew`, `/v1/clients`, `/v1/tokens`
- CLI: `cf controller init`, `cf controller token create`, `cf connect`, `cf client list|remove`, `cf config paths|show`

**No entra:**
- Identidades de Worker (llegan en worker-registry-heartbeat)

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Init
- **Given** un host limpio
- **When** se ejecuta `cf controller init`
- **Then** crea CA, certificado de servidor, DB, bundle del owner con `0600` y una invitación; repetirlo no sobrescribe y da un error claro

### Scenario: Fingerprint
- **Given** una invitación con fingerprint F y un servidor con otra CA
- **When** se ejecuta `cf connect`
- **Then** aborta antes de enviar el token

### Scenario: Canje atómico
- **Given** un token
- **When** hay dos canjes concurrentes
- **Then** exactamente uno obtiene certificado y el otro recibe `CF_UNAUTHENTICATED`

### Scenario: Revocación
- **Given** un cliente revocado
- **When** hace cualquier llamada
- **Then** `CF_UNAUTHENTICATED` y exit 6

### Scenario: Renovación
- **Given** un certificado válido / caducado
- **When** se ejecuta `renew`
- **Then** recibe uno nuevo / `CF_UNAUTHENTICATED` con la indicación de pedir una invitación

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §16.6, §35, §36.1

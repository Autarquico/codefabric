# Distribución con Homebrew

`codefabric` se publica en un tap propio, `autarquico/homebrew-tap`, como la fórmula
**`codefabric`**, que instala el binario `cf`. Plataformas: macOS arm64, Linux amd64 y Linux arm64
(las mismas que `install.sh`, sin darwin/amd64).

## La fórmula es un artefacto derivado

`codefabric.rb.tmpl` no lleva versiones ni digests escritos a mano. `packaging/release/brew-formula.sh`
la renderiza desde el `checksums.txt` **firmado** de una release:

```sh
packaging/release/brew-formula.sh checksums.txt install.sh vX.Y.Z > codefabric.rb
```

- Verifica la firma de `checksums.txt` (`checksums.txt.sig`, vía `verify-asset.sh`) antes de leer nada.
  `TRUST` es un fichero `allowed_signers` o el `install.sh` del checkout del repo en el commit del tag
  (nunca el `install.sh` descargado de la release: la verificación sería circular).
- La cabecera `version: vX.Y.Z` debe coincidir con `VERSION` (opcional) y no puede llevar sufijo
  (`-rc.1`, etc.): las prereleases no tienen fórmula.
- Cada plataforma debe aparecer exactamente una vez con un sha256 de 64 hex.
- Se renderiza a un fichero temporal y solo entonces se escribe en stdout: un error no deja salida parcial.
- `CF_RELEASE_BASE_URL` cambia la base de descarga (solo para tests o mirrors).

## Publicar (paso humano)

CI no tiene token del tap. Los pasos están en `docs/runbooks/release.md`.

## Verificación en este repositorio

`make test-installer` ejecuta `TestScenario_FormulaDerivada_*` (`packaging/release/test/brew_test.sh`).
Si hay `ruby`, además comprueba la sintaxis de la fórmula renderizada. `brew style`/`brew audit` solo tienen sentido
dentro del tap (fuera aplican cops de Sorbet que no corresponden a una fórmula): se ejecutan al publicar.

# Distribución con Homebrew

Code Flock se publica en un tap propio, `autarquico/homebrew-tap`, como la fórmula
**`codeflock`**, que instala solo el binario `cflk` (no el `cf` de transición: choca con `cf-cli`). Los assets son
`cflk_<ver>_<os>_<arch>.tar.gz` de `Autarquico/codeflock`. Plataformas: macOS arm64 y amd64, Linux amd64 y Linux arm64
(las mismas que `install.sh`).

## La fórmula es un artefacto derivado

`codeflock.rb.tmpl` no lleva versiones ni digests escritos a mano. `packaging/release/brew-formula.sh`
la renderiza desde el `checksums.txt` **firmado** de una release:

```sh
packaging/release/brew-formula.sh checksums.txt install.sh vX.Y.Z > codeflock.rb
```

- Verifica la firma de `checksums.txt` (`checksums.txt.sig`, vía `verify-asset.sh`) antes de leer nada.
  `TRUST` es un fichero `allowed_signers` o el `install.sh` del checkout del repo en el commit del tag
  (nunca el `install.sh` descargado de la release: la verificación sería circular).
- La cabecera `version: vX.Y.Z` debe coincidir con `VERSION` (opcional) y no puede llevar sufijo
  (`-rc.1`, etc.): las prereleases no tienen fórmula.
- Cada plataforma debe aparecer exactamente una vez con un sha256 de 64 hex.
- Se renderiza a un fichero temporal y solo entonces se escribe en stdout: un error no deja salida parcial.
- `CFLK_RELEASE_BASE_URL` cambia la base de descarga (solo para tests o mirrors).

## La fórmula antigua `codefabric`

`formula_renames.json` (`{"codefabric": "codeflock"}`) va en la raíz del tap, junto a `Formula/`: es el mecanismo
que documenta Homebrew para renombrar, y hace que `brew upgrade` de quien tiene `codefabric` instale `codeflock`.
En el tap se borra `Formula/codefabric.rb` y se añade `Formula/codeflock.rb`. No se publica hasta la primera
versión estable (el renderer rechaza prereleases); ese paso valida el scenario "Homebrew" de `rename-cli`.

## Publicar (paso humano)

CI no tiene token del tap. Los pasos están en `docs/runbooks/release.md`.

## Verificación en este repositorio

`make test-installer` ejecuta `TestScenario_FormulaDerivada_*` (`packaging/release/test/brew_test.sh`).
Si hay `ruby`, además comprueba la sintaxis de la fórmula renderizada. `brew style`/`brew audit` solo tienen sentido
dentro del tap (fuera aplican cops de Sorbet que no corresponden a una fórmula): se ejecutan al publicar.

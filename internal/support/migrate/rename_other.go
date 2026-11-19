//go:build !linux && !darwin

package migrate

import "errors"

func renameNoReplace(string, string) error {
	return errors.New("migrate: a move that never replaces needs Linux or macOS")
}

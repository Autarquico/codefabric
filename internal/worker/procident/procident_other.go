//go:build !linux && !darwin

package procident

func startOf(int) (string, error) { return "", ErrUnverifiable }

func bootOf() (string, error) { return "", ErrUnverifiable }

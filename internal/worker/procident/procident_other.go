//go:build !linux && !darwin

package procident

import "errors"

func startOf(int) (string, error) { return "", ErrUnverifiable }

func bootOf() (string, error) { return "", ErrUnverifiable }

func groupMembers(int) ([]int, error) {
	return nil, errors.New("procident: cannot list a process group on this platform")
}

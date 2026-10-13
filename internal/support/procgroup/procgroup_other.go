//go:build !linux && !darwin

package procgroup

import "errors"

var errUnsupported = errors.New("procgroup: cannot list a process group on this platform")

func members(int) ([]int, error) { return nil, errUnsupported }

func running(int) (bool, error) { return false, errUnsupported }

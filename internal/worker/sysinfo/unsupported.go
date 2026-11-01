//go:build !linux && !darwin

package sysinfo

import "errors"

var errUnsupported = errors.New("sysinfo: this platform is not supported")

func memoryBytes() (uint64, error)         { return 0, errUnsupported }
func diskBytes(string) (uint64, error)     { return 0, errUnsupported }
func diskFreeBytes(string) (uint64, error) { return 0, errUnsupported }

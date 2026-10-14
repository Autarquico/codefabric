package svcunit

import "runtime"

// Replaceable is replaceable with the groups this machine trusts for uid:
// another uid makes the files of this user someone else's.
func Replaceable(program string, uid int) error {
	return replaceable(program, uid, trustedGroupOf(runtime.GOOS, uid))
}

// ReplaceableTrusting is replaceable with the groups that may write said by trusted.
var ReplaceableTrusting = replaceable

var TrustedGroup = trustedGroup

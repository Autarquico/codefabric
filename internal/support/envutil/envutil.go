// Package envutil holds helpers over process environments.
package envutil

import "strings"

// WithoutTmux is env without the variables that make a process believe it is in
// a tmux client (TMUX, TMUX_PANE). It does not change env.
func WithoutTmux(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_PANE=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

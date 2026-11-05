package notify

import (
	"context"
	"strings"
)

// Native shows a notification in the desktop of this machine.
type Native interface {
	Show(ctx context.Context, title, body string) error
}

// LookPath is exec.LookPath, injectable.
type LookPath func(file string) (string, error)

// DetectNative picks the adapter for goos, or nil if this machine has none
// (macOS has osascript; Linux has it if notify-send is installed).
func DetectNative(goos string, look LookPath, run Exec) Native {
	switch goos {
	case "darwin":
		if _, err := look("osascript"); err == nil {
			return osascript{run}
		}
	case "linux":
		if _, err := look("notify-send"); err == nil {
			return notifySend{run}
		}
	}
	return nil
}

// appleScript reads the title and the body from argv: the text is never part of the script.
var appleScript = []string{
	"on run argv",
	"display notification (item 2 of argv) with title (item 1 of argv)",
	"end run",
}

type osascript struct{ run Exec }

func (o osascript) Show(ctx context.Context, title, body string) error {
	_, err := o.run(ctx, "osascript", osascriptArgs(title, body), nil)
	return err
}

func osascriptArgs(title, body string) []string {
	var args []string
	for _, line := range appleScript {
		args = append(args, "-e", line)
	}
	return append(args, notOption(title), notOption(body))
}

// notOption keeps osascript from reading a leading dash as one of its options.
func notOption(s string) string {
	if strings.HasPrefix(s, "-") {
		return " " + s
	}
	return s
}

type notifySend struct{ run Exec }

func (n notifySend) Show(ctx context.Context, title, body string) error {
	_, err := n.run(ctx, "notify-send", notifySendArgs(title, body), nil)
	return err
}

func notifySendArgs(title, body string) []string {
	return []string{"--app-name=CodeFabric", "--", title, body}
}

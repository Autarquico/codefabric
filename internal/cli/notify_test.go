package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type failingNative struct{}

func (failingNative) Show(context.Context, string, string) error {
	return errors.New("osascript failed")
}

type workingNative struct{ shown int }

func (w *workingNative) Show(context.Context, string, string) error { w.shown++; return nil }

func TestNotifyTestReportsTheRealResult(t *testing.T) {
	t.Setenv("CF_CONFIG_DIR", t.TempDir())
	run := func(opts Options) (int, string, string) {
		var out, errOut strings.Builder
		code := RunContextWith(context.Background(), opts, []string{"notify", "test"}, strings.NewReader(""), &out, &errOut)
		return code, out.String(), errOut.String()
	}

	if code, out, errOut := run(Options{}); code == 0 || strings.Contains(out, "sent") || !strings.Contains(errOut, "no native notifications") {
		t.Errorf("nothing to deliver with: exit %d, %q, %q", code, out, errOut)
	}
	if code, out, errOut := run(Options{Native: failingNative{}}); code == 0 || strings.Contains(out, "sent") || !strings.Contains(errOut, "osascript failed") {
		t.Errorf("failed delivery: exit %d, %q, %q", code, out, errOut)
	}
	native := &workingNative{}
	if code, out, errOut := run(Options{Native: native}); code != 0 || !strings.Contains(out, "sent") || native.shown != 1 {
		t.Errorf("delivered: exit %d, %q, %q, shown %d", code, out, errOut, native.shown)
	}
}

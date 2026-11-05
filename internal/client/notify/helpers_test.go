package notify_test

import (
	"io"
	"log/slog"
)

func newLogger(w io.Writer) *slog.Logger { return slog.New(slog.NewTextHandler(w, nil)) }

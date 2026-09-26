package cmd

import (
	"bytes"
	"log/slog"
	"testing"
)

func TestPlainWarnings(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	var buf bytes.Buffer
	setupLogging(&buf, false)
	slog.Info("hidden")
	slog.With("host", "web01").Warn("slow", "ms", 1200)
	slog.Error("broken")
	want := "mrsh: warning: slow host=web01 ms=1200\nmrsh: error: broken\n"
	if buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
	}
}

func TestFullOutputAccepted(t *testing.T) {
	out, err := execRoot(t, "-o", "full", "version")
	if err != nil || out == "" {
		t.Fatalf("err=%v out=%q", err, out)
	}
}

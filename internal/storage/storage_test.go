package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNewSinkDispatch(t *testing.T) {
	tests := []struct {
		uri  string
		want string // local dir, "s3:<bucket>|<prefix>" or "error"
	}{
		{"backups/", "backups/"},
		{"./backups", "./backups"},
		{"/var/lib/mrsh/backups/", "/var/lib/mrsh/backups/"},
		{"file:///var/backups", "/var/backups"},
		{`C:\backups`, `C:\backups`},
		{"s3://bucket/mrsh/backups/", "s3:bucket|mrsh/backups"},
		{"s3://bucket", "s3:bucket|"},
		{"s3:///prefix", "error"},
		{"gs://bucket/x", "error"},
		{"file://host/x", "error"},
		{"", "error"},
	}
	for _, tt := range tests {
		s, err := NewSink(tt.uri, SinkOptions{SSE: "AES256"})
		got := "error"
		switch {
		case err != nil:
		case isS3(s):
			got = "s3:" + s.(*S3Sink).Bucket + "|" + s.(*S3Sink).Prefix
		default:
			got = s.(*LocalSink).Dir
		}
		if got != tt.want {
			t.Errorf("NewSink(%q) = %s (err %v), want %s", tt.uri, got, err, tt.want)
		}
	}
}

func TestLocalSinkWrite(t *testing.T) {
	dir := t.TempDir()
	s := &LocalSink{Dir: dir}
	n, err := s.Write(context.Background(), "web/web01/2026-04-03.rsc", strings.NewReader("/ip address\n"))
	if err != nil || n != 12 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	p := filepath.Join(dir, "web", "web01", "2026-04-03.rsc")
	raw, err := os.ReadFile(p)
	if err != nil || string(raw) != "/ip address\n" {
		t.Fatalf("read %q %v", raw, err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v", fi.Mode().Perm())
		}
	}
	// Overwrite the same day.
	if _, err := s.Write(context.Background(), "web/web01/2026-04-03.rsc", strings.NewReader("v2")); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(p); string(raw) != "v2" {
		t.Errorf("overwrite = %q", raw)
	}
	entries, _ := os.ReadDir(filepath.Dir(p))
	if len(entries) != 1 {
		t.Errorf("temp files left behind: %v", entries)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("sftp broke") }

func TestLocalSinkFailedStreamLeavesNoFile(t *testing.T) {
	dir := t.TempDir()
	s := &LocalSink{Dir: dir}
	if _, err := s.Write(context.Background(), "g/n/x.backup", failingReader{}); err == nil {
		t.Fatal("expected error")
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "g", "n"))
	if len(entries) != 0 {
		t.Errorf("partial file left: %v", entries)
	}
}

func TestCleanKey(t *testing.T) {
	for _, bad := range []string{"", "/abs", "../x", "a/../../x", "a//b", "a/./b", `a\b`} {
		if _, err := CleanKey(bad); err == nil {
			t.Errorf("CleanKey(%q) accepted", bad)
		}
	}
	if k, err := CleanKey("web/web01/2026-04-03.rsc"); err != nil || k != "web/web01/2026-04-03.rsc" {
		t.Errorf("good key: %q %v", k, err)
	}
}

func isS3(s Sink) bool { _, ok := s.(*S3Sink); return ok }

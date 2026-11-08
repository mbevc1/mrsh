package config

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

func TestNewStore(t *testing.T) {
	tests := []struct {
		uri  string
		want string // "local:<path>", "s3:<bucket>/<key>" or "error"
	}{
		{"hosts.yaml", "local:hosts.yaml"},
		{"/etc/mrsh/hosts.yaml", "local:/etc/mrsh/hosts.yaml"},
		{"file:///etc/mrsh/hosts.yaml", "local:/etc/mrsh/hosts.yaml"},
		{`C:\mrsh\hosts.yaml`, `local:C:\mrsh\hosts.yaml`},
		{"s3://bucket/mrsh/hosts.yaml", "s3:bucket/mrsh/hosts.yaml"},
		{"s3://bucket/", "error"},
		{"s3:///key", "error"},
		{"gs://bucket/key", "error"},
		{"file://remote/x", "error"},
		{"", "error"},
	}
	for _, tt := range tests {
		s, err := NewStore(tt.uri)
		got := "error"
		switch s := s.(type) {
		case *LocalStore:
			got = "local:" + s.Path
		case *S3Store:
			got = "s3:" + s.Bucket + "/" + s.Key
		}
		if err != nil {
			got = "error"
		}
		if got != tt.want {
			t.Errorf("NewStore(%q) = %s (err %v), want %s", tt.uri, got, err, tt.want)
		}
	}
}

func TestS3StoreNotImplemented(t *testing.T) {
	s := &S3Store{Bucket: "b", Key: "k"}
	if _, _, err := s.Load(context.Background()); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("Load err = %v", err)
	}
	if err := s.Save(context.Background(), nil, ""); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("Save err = %v", err)
	}
}

func TestLocalStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := &LocalStore{Path: filepath.Join(t.TempDir(), "hosts.yaml")}

	if _, _, err := s.Load(ctx); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Load missing file err = %v", err)
	}
	if err := s.Save(ctx, []byte("v1"), "stale"); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("update of missing file err = %v", err)
	}
	if err := s.Save(ctx, []byte("v1"), ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, []byte("again"), ""); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("create-only over existing file err = %v", err)
	}

	raw, ver, err := s.Load(ctx)
	if err != nil || string(raw) != "v1" || ver == "" {
		t.Fatalf("Load = %q, %q, %v", raw, ver, err)
	}
	if err := s.Save(ctx, []byte("v2"), ver); err != nil {
		t.Fatal(err)
	}
	// The v1 version is now stale.
	if err := s.Save(ctx, []byte("v3"), ver); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale save err = %v", err)
	}
	if raw, _, _ := s.Load(ctx); string(raw) != "v2" {
		t.Errorf("content = %q, want v2", raw)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(s.Path)
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("new file mode = %v, want 0600", fi.Mode().Perm())
		}
	}
}

func TestLocalStorePreservesMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions")
	}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "hosts.yaml")
	if err := os.WriteFile(path, []byte("v1"), 0o640); err != nil {
		t.Fatal(err)
	}
	s := &LocalStore{Path: path}
	_, ver, _ := s.Load(ctx)
	if err := s.Save(ctx, []byte("v2"), ver); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", fi.Mode().Perm())
	}
}

func TestLocalStoreConcurrentSaves(t *testing.T) {
	ctx := context.Background()
	s := &LocalStore{Path: filepath.Join(t.TempDir(), "hosts.yaml")}
	if err := s.Save(ctx, []byte("base"), ""); err != nil {
		t.Fatal(err)
	}
	_, ver, _ := s.Load(ctx)

	const writers = 8
	var wins, conflicts atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each writer uses its own store value, as separate processes would.
			err := (&LocalStore{Path: s.Path}).Save(ctx, []byte(fmt.Sprintf("writer %d", i)), ver)
			switch {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, ErrVersionConflict):
				conflicts.Add(1)
			default:
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 || conflicts.Load() != writers-1 {
		t.Errorf("wins=%d conflicts=%d, want 1 and %d", wins.Load(), conflicts.Load(), writers-1)
	}
}

// Package storage provides Sink implementations for writing files locally or to S3.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Sink stores a stream under a relative, slash-separated key such as
// "web/web01/2026-04-03.rsc". Keys are identical across sink types.
type Sink interface {
	Write(ctx context.Context, key string, r io.Reader) (int64, error)
	// Location describes the sink for logs and output.
	Location() string
	// Preflight checks the destination is reachable before any device work.
	Preflight(ctx context.Context) error
}

// SinkOptions are S3-only settings; local sinks ignore them.
type SinkOptions struct {
	SSE    string // "AES256" or "aws:kms"
	KMSKey string // implies SSE aws:kms
}

// NewSink dispatches on the URI scheme: none or "file" gives a LocalSink,
// "s3" an S3Sink.
func NewSink(uri string, opts SinkOptions) (Sink, error) {
	if uri == "" {
		return nil, errors.New("storage path is empty")
	}
	if len(uri) >= 2 && uri[1] == ':' && isLetter(uri[0]) { // Windows drive
		return newLocal(uri, opts), nil
	}
	u, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("invalid storage path %q: %w", uri, err)
	}
	switch u.Scheme {
	case "":
		return newLocal(uri, opts), nil
	case "file":
		if u.Host != "" && u.Host != "localhost" {
			return nil, fmt.Errorf("invalid storage path %q: file URI host must be empty", uri)
		}
		return newLocal(u.Path, opts), nil
	case "s3":
		if u.Host == "" {
			return nil, fmt.Errorf("invalid storage path %q: want s3://bucket/prefix/", uri)
		}
		return newS3Sink(uri, opts)
	}
	return nil, fmt.Errorf("invalid storage path %q: unsupported scheme %q", uri, u.Scheme)
}

func isLetter(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') }

func newLocal(dir string, opts SinkOptions) *LocalSink {
	if opts.SSE != "" || opts.KMSKey != "" {
		slog.Debug("--sse/--kms-key ignored for a local path", "path", dir)
	}
	return &LocalSink{Dir: dir}
}

// LocalSink writes files under Dir, creating parent directories. Each file
// appears atomically: it is written to a temp name and renamed.
type LocalSink struct {
	Dir string
}

func (s *LocalSink) Location() string { return s.Dir }

// Preflight creates the base directory.
func (s *LocalSink) Preflight(context.Context) error { return os.MkdirAll(s.Dir, 0o750) }

func (s *LocalSink) Write(ctx context.Context, key string, r io.Reader) (int64, error) {
	clean, err := CleanKey(key)
	if err != nil {
		return 0, err
	}
	dst := filepath.Join(s.Dir, filepath.FromSlash(clean))
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp-*")
	if err != nil {
		return 0, err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // no-op after rename

	n, err := io.Copy(tmp, readerWithContext(ctx, r))
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(name, 0o600) // backups hold device secrets
	}
	if err == nil {
		err = os.Rename(name, dst)
	}
	if err != nil {
		return n, fmt.Errorf("write %s: %w", dst, err)
	}
	slog.Debug("sink write", "sink", "local", "dest", dst, "bytes", n)
	return n, nil
}

// CleanKey validates a relative slash-separated key and rejects anything
// that could escape the sink root.
func CleanKey(key string) (string, error) {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "\\") {
		return "", fmt.Errorf("invalid storage key %q", key)
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("invalid storage key %q", key)
		}
	}
	return path.Clean(key), nil
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func readerWithContext(ctx context.Context, r io.Reader) io.Reader { return &ctxReader{ctx, r} }

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// lockRetry is how often Save retries a held lock until ctx expires.
const lockRetry = 20 * time.Millisecond

var (
	// ErrVersionConflict means the config changed since it was loaded (or
	// already exists, for a create-only save). Reload and reapply.
	ErrVersionConflict = errors.New("config changed since it was loaded; reload and retry")
	// ErrNotImplemented is returned by stores not built yet.
	ErrNotImplemented = errors.New("not implemented")
)

// ConfigStore reads and conditionally writes the raw config, wherever it lives.
type ConfigStore interface {
	// Load returns the raw config and an opaque version token.
	Load(ctx context.Context) (raw []byte, version string, err error)
	// Save writes raw only if the stored version still equals ifVersion.
	// An empty ifVersion means create-only: fail if the config exists.
	Save(ctx context.Context, raw []byte, ifVersion string) error
	// Location describes the store for logs and errors.
	Location() string
}

// NewStore dispatches on the URI scheme: none or "file" gives a LocalStore,
// "s3" an S3Store.
func NewStore(uri string) (ConfigStore, error) {
	if uri == "" {
		return nil, errors.New("config location is empty")
	}
	// Windows drive letters ("C:\...") parse as a scheme; treat them as paths.
	if len(uri) >= 2 && uri[1] == ':' && isLetter(uri[0]) {
		return &LocalStore{Path: uri}, nil
	}
	u, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("invalid config location %q: %w", uri, err)
	}
	switch u.Scheme {
	case "":
		return &LocalStore{Path: uri}, nil
	case "file":
		p := u.Path
		if u.Host != "" && u.Host != "localhost" {
			return nil, fmt.Errorf("invalid config location %q: file URI host must be empty", uri)
		}
		if p == "" {
			p = u.Opaque
		}
		return &LocalStore{Path: p}, nil
	case "s3":
		key := strings.TrimPrefix(u.Path, "/")
		if u.Host == "" || key == "" || strings.HasSuffix(key, "/") {
			return nil, fmt.Errorf("invalid config location %q: want s3://bucket/key", uri)
		}
		return &S3Store{Bucket: u.Host, Key: key}, nil
	}
	return nil, fmt.Errorf("invalid config location %q: unsupported scheme %q", uri, u.Scheme)
}

func isLetter(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') }

// LocalStore keeps the config in a local file. The version is the SHA-256 of
// the content; Save compares it under an exclusive lock on "<path>.lock", so
// concurrent writers (CLI and UI) cannot silently clobber each other.
type LocalStore struct {
	Path string
}

func (s *LocalStore) Location() string { return s.Path }

func (s *LocalStore) Load(_ context.Context) ([]byte, string, error) {
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, "", err
	}
	return raw, digest(raw), nil
}

func (s *LocalStore) Save(ctx context.Context, raw []byte, ifVersion string) error {
	lock := flock.New(s.Path + ".lock")
	if _, err := lock.TryLockContext(ctx, lockRetry); err != nil {
		return fmt.Errorf("lock %s: %w", s.Path, err)
	}
	defer func() { _ = lock.Unlock() }()

	mode := fs.FileMode(0o600)
	cur, err := os.ReadFile(s.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if ifVersion != "" {
			return ErrVersionConflict
		}
	case err != nil:
		return err
	default:
		if ifVersion == "" || digest(cur) != ifVersion {
			return ErrVersionConflict
		}
		if fi, err := os.Stat(s.Path); err == nil {
			mode = fi.Mode().Perm()
		}
	}
	return writeAtomic(s.Path, raw, mode)
}

// writeAtomic writes via a temp file in the same directory, then renames.
func writeAtomic(path string, raw []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // no-op once renamed

	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// S3Store keeps the config in an S3 object with ETag optimistic locking.
// Implemented in Phase 6.
type S3Store struct {
	Bucket string
	Key    string
}

func (s *S3Store) Location() string { return "s3://" + s.Bucket + "/" + s.Key }

func (s *S3Store) Load(context.Context) ([]byte, string, error) {
	return nil, "", fmt.Errorf("s3 config store: %w", ErrNotImplemented)
}

func (s *S3Store) Save(context.Context, []byte, string) error {
	return fmt.Errorf("s3 config store: %w", ErrNotImplemented)
}

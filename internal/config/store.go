package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/mbevc1/mrsh/internal/s3client"
)

var (
	// ErrVersionConflict means the config changed since it was loaded (or
	// already exists, for a create-only save). Reload and reapply.
	ErrVersionConflict = errors.New("config changed since it was loaded; reload and retry")
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

// StoreOptions are S3-only settings; local stores ignore them.
type StoreOptions struct {
	SSE s3client.SSE // encryption for S3Store.Save
}

// NewStore dispatches on the URI scheme: none or "file" gives a LocalStore,
// "s3" an S3Store.
func NewStore(uri string, opts StoreOptions) (ConfigStore, error) {
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
		return &S3Store{Bucket: u.Host, Key: key, SSE: opts.SSE}, nil
	}
	return nil, fmt.Errorf("invalid config location %q: unsupported scheme %q", uri, u.Scheme)
}

func isLetter(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') }

// LocalStore keeps the config in a local file, versioned by the SHA-256 of
// its content: Save refuses when the file changed since it was loaded.
// Saves within one process are serialized per path; saves from two
// processes in the same few microseconds are not.
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

func (s *LocalStore) Save(_ context.Context, raw []byte, ifVersion string) error {
	mu := pathMutex(s.Path)
	mu.Lock()
	defer mu.Unlock()

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

var saveMutexes sync.Map // absolute path -> *sync.Mutex

func pathMutex(path string) *sync.Mutex {
	key, err := filepath.Abs(path)
	if err != nil {
		key = filepath.Clean(path)
	}
	mu, _ := saveMutexes.LoadOrStore(key, &sync.Mutex{})
	return mu.(*sync.Mutex)
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

// S3Store keeps the config in an S3 object. The version is the object
// ETag; Save is a conditional PutObject (If-Match, or If-None-Match: * for
// create-only), so a concurrent writer on another machine is detected
// instead of silently overwritten.
type S3Store struct {
	Bucket string
	Key    string
	SSE    s3client.SSE

	mu     sync.Mutex
	client *s3.Client
}

func (s *S3Store) Location() string { return "s3://" + s.Bucket + "/" + s.Key }

func (s *S3Store) api(ctx context.Context) (*s3.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil {
		c, err := s3client.New(ctx, s.Bucket)
		if err != nil {
			return nil, err
		}
		s.client = c
	}
	return s.client, nil
}

func (s *S3Store) Load(ctx context.Context) ([]byte, string, error) {
	c, err := s.api(ctx)
	if err != nil {
		return nil, "", err
	}
	out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.Bucket), Key: aws.String(s.Key)})
	var noKey *s3types.NoSuchKey
	if errors.As(err, &noKey) || httpStatus(err) == http.StatusNotFound {
		return nil, "", fmt.Errorf("%s: %w", s.Location(), fs.ErrNotExist)
	}
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = out.Body.Close() }()
	raw, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, "", err
	}
	return raw, aws.ToString(out.ETag), nil
}

func (s *S3Store) Save(ctx context.Context, raw []byte, ifVersion string) error {
	c, err := s.api(ctx)
	if err != nil {
		return err
	}
	mode, kmsKey := s.SSE.Apply()
	in := &s3.PutObjectInput{
		Bucket:               aws.String(s.Bucket),
		Key:                  aws.String(s.Key),
		Body:                 bytes.NewReader(raw),
		ContentType:          aws.String("application/yaml"),
		ServerSideEncryption: mode,
		SSEKMSKeyId:          kmsKey,
	}
	if ifVersion == "" {
		in.IfNoneMatch = aws.String("*")
	} else {
		in.IfMatch = aws.String(ifVersion)
	}
	_, err = c.PutObject(ctx, in)
	switch httpStatus(err) {
	case http.StatusPreconditionFailed, http.StatusConflict:
		return ErrVersionConflict
	}
	return err
}

func httpStatus(err error) int {
	var re *awshttp.ResponseError
	if errors.As(err, &re) {
		return re.HTTPStatusCode()
	}
	return 0
}

package storage

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/mbevc1/mrsh/internal/s3client"
)

// DefaultPartSize is the multipart part size; memory use per upload is
// about PartSize × upload concurrency, never the whole file.
const DefaultPartSize = manager.DefaultUploadPartSize

// S3Sink streams objects to s3://Bucket/Prefix/<key> with multipart uploads.
type S3Sink struct {
	Bucket   string
	Prefix   string
	SSE      s3client.SSE
	PartSize int64

	mu       sync.Mutex
	client   *s3.Client
	uploader *manager.Uploader //nolint:staticcheck // see newUploader
}

func (s *S3Sink) Location() string { return "s3://" + path.Join(s.Bucket, s.Prefix) + "/" }

// Preflight confirms the bucket exists and is reachable with the current
// credentials, so a bad path fails before any device work.
func (s *S3Sink) Preflight(ctx context.Context) error {
	if _, err := s.newUploader(ctx); err != nil {
		return err
	}
	if _, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.Bucket)}); err != nil {
		return fmt.Errorf("s3://%s: %w", s.Bucket, err)
	}
	return nil
}

// newUploader builds the SDK uploader once per sink. manager.Uploader is
// deprecated in favour of feature/s3/transfermanager, which is still v0.x
// with no API stability promise; switch once it reaches v1.
func (s *S3Sink) newUploader(ctx context.Context) (*manager.Uploader, error) { //nolint:staticcheck // see above
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.uploader != nil {
		return s.uploader, nil
	}
	client, err := s3client.New(ctx, s.Bucket)
	if err != nil {
		return nil, err
	}
	s.client = client
	s.uploader = manager.NewUploader(client, func(u *manager.Uploader) { //nolint:staticcheck // see above
		if s.PartSize > 0 {
			u.PartSize = s.PartSize
		}
	})
	return s.uploader, nil
}

// Write uploads r under key without buffering the whole stream.
func (s *S3Sink) Write(ctx context.Context, key string, r io.Reader) (int64, error) {
	clean, err := CleanKey(key)
	if err != nil {
		return 0, err
	}
	objKey := clean
	if s.Prefix != "" {
		objKey = s.Prefix + "/" + clean
	}
	up, err := s.newUploader(ctx)
	if err != nil {
		return 0, err
	}
	mode, kmsKey := s.SSE.Apply()
	counter := &countingReader{r: r}
	start := time.Now()
	_, err = up.Upload(ctx, &s3.PutObjectInput{ //nolint:staticcheck // see newUploader
		Bucket:               aws.String(s.Bucket),
		Key:                  aws.String(objKey),
		Body:                 counter,
		ServerSideEncryption: mode,
		SSEKMSKeyId:          kmsKey,
	})
	n := counter.n.Load()
	if err != nil {
		return n, fmt.Errorf("upload s3://%s/%s: %w", s.Bucket, objKey, err)
	}
	partSize := up.PartSize
	parts := (n + partSize - 1) / partSize
	if parts == 0 {
		parts = 1
	}
	slog.Debug("sink write", "sink", "s3", "bucket", s.Bucket, "key", objKey, "bytes", n,
		"parts", parts, "sse", string(mode), "latency", time.Since(start))
	return n, nil
}

type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

func newS3Sink(uri string, opts SinkOptions) (*S3Sink, error) {
	loc, err := s3client.Parse(uri)
	if err != nil {
		return nil, err
	}
	sse := s3client.SSE{Mode: opts.SSE, KMSKey: opts.KMSKey}
	if err := sse.Validate(); err != nil {
		return nil, err
	}
	return &S3Sink{Bucket: loc.Bucket, Prefix: strings.Trim(loc.Key, "/"), SSE: sse}, nil
}

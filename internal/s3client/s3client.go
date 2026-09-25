// Package s3client builds the S3 client shared by the config store and the
// backup sink, using the standard AWS SDK credential chain.
package s3client

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// New returns a client for bucket. When no region is configured
// (AWS_REGION, profile), it asks S3 where the bucket lives. A custom
// endpoint (AWS_ENDPOINT_URL_S3 or AWS_ENDPOINT_URL, e.g. MinIO) switches
// to path-style addressing.
func New(ctx context.Context, bucket string) (*s3.Client, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	pathStyle := func(o *s3.Options) {
		if o.BaseEndpoint != nil {
			o.UsePathStyle = true
		}
	}
	if cfg.Region == "" {
		probe := s3.NewFromConfig(cfg, pathStyle, func(o *s3.Options) { o.Region = "us-east-1" })
		start := time.Now()
		region, err := manager.GetBucketRegion(ctx, probe, bucket)
		if err != nil {
			return nil, fmt.Errorf("detect region of bucket %s (set AWS_REGION to skip): %w", bucket, err)
		}
		slog.Debug("s3 bucket region detected", "bucket", bucket, "region", region, "latency", time.Since(start))
		cfg.Region = region
	}
	return s3.NewFromConfig(cfg, pathStyle), nil
}

// Location is a parsed s3://bucket/key-or-prefix URI.
type Location struct {
	Bucket string
	Key    string
}

// Parse splits an s3:// URI. The key may be empty (bucket root).
func Parse(uri string) (Location, error) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "s3" || u.Host == "" {
		return Location{}, fmt.Errorf("invalid S3 URI %q: want s3://bucket/key", uri)
	}
	return Location{Bucket: u.Host, Key: strings.TrimPrefix(u.Path, "/")}, nil
}

// SSE holds server-side encryption settings for writes.
type SSE struct {
	Mode   string // "", "AES256" or "aws:kms"
	KMSKey string // implies aws:kms
}

// Apply returns the SDK values for SSE; a KMS key implies aws:kms.
func (s SSE) Apply() (types.ServerSideEncryption, *string) {
	if s.KMSKey != "" {
		return types.ServerSideEncryptionAwsKms, aws.String(s.KMSKey)
	}
	return types.ServerSideEncryption(s.Mode), nil
}

// Validate checks the mode.
func (s SSE) Validate() error {
	switch s.Mode {
	case "", "AES256", "aws:kms":
		return nil
	}
	return fmt.Errorf("invalid --sse %q: must be AES256 or aws:kms", s.Mode)
}

package storage

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/mbevc1/mrsh/internal/s3client"
	"github.com/mbevc1/mrsh/internal/s3test"
)

func TestS3SinkSmallObjectLayoutAndSSE(t *testing.T) {
	fake := s3test.Start(t, "backups")
	sink, err := NewSink("s3://backups/mrsh/prod/", SinkOptions{SSE: "AES256"})
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	n, err := sink.Write(context.Background(), "lab/rt1/2026-04-06.rsc", strings.NewReader("/ip address\n"))
	if err != nil || n != 12 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	o := fake.Object("backups", "mrsh/prod/lab/rt1/2026-04-06.rsc")
	if o == nil || string(o.Data) != "/ip address\n" || o.SSE != "AES256" {
		t.Fatalf("object = %+v", o)
	}
	if got := sink.Location(); got != "s3://backups/mrsh/prod/" {
		t.Errorf("Location = %q", got)
	}
}

// A body larger than one part goes up as a multipart upload in bounded
// pieces, streamed from an unseekable reader.
func TestS3SinkStreamsMultipartWithKMS(t *testing.T) {
	fake := s3test.Start(t, "backups")
	const part = 5 << 20 // S3 minimum part size
	sink := &S3Sink{Bucket: "backups", SSE: s3client.SSE{KMSKey: "arn:aws:kms:eu-west-1:123456789012:key/abc"}, PartSize: part}

	payload := bytes.Repeat([]byte("0123456789abcdef"), (12<<20)/16) // 12 MiB
	pr, pw := io.Pipe()
	go func() {
		for off := 0; off < len(payload); off += 64 << 10 {
			end := min(off+64<<10, len(payload))
			if _, err := pw.Write(payload[off:end]); err != nil {
				return
			}
		}
		_ = pw.Close()
	}()
	n, err := sink.Write(context.Background(), "g/n/2026-04-06.backup", pr)
	if err != nil || n != int64(len(payload)) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	o := fake.Object("backups", "g/n/2026-04-06.backup")
	if o == nil || !bytes.Equal(o.Data, payload) {
		t.Fatal("assembled object differs from payload")
	}
	if o.SSE != "aws:kms" || o.KMSKey != "arn:aws:kms:eu-west-1:123456789012:key/abc" {
		t.Errorf("SSE = %q / %q", o.SSE, o.KMSKey)
	}
	parts, biggest := 0, 0
	for _, r := range fake.Requests() {
		if r.Op == "UploadPart" {
			parts++
			biggest = max(biggest, r.BodyLen)
		}
		if r.Op == "PutObject" {
			t.Error("large body used a single PutObject")
		}
	}
	if parts != 3 || biggest > part {
		t.Errorf("parts=%d biggest=%d, want 3 parts of at most %d bytes", parts, biggest, part)
	}
}

func TestS3SinkMissingBucketFailsPreflight(t *testing.T) {
	s3test.Start(t, "other")
	sink, _ := NewSink("s3://missing/prefix/", SinkOptions{})
	if err := sink.Preflight(context.Background()); err == nil {
		t.Error("preflight passed for a missing bucket")
	}
}

func TestS3SinkBadSSE(t *testing.T) {
	if _, err := NewSink("s3://b/p/", SinkOptions{SSE: "rot13"}); err == nil {
		t.Error("bad SSE accepted")
	}
}

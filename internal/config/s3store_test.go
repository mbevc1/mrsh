package config

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"testing"

	"github.com/mbevc1/mrsh/internal/s3client"
	"github.com/mbevc1/mrsh/internal/s3test"
)

func TestS3StoreETagLocking(t *testing.T) {
	fake := s3test.Start(t, "cfg")
	ctx := context.Background()
	store, err := NewStore("s3://cfg/mrsh/hosts.yaml", StoreOptions{SSE: s3client.SSE{Mode: "AES256"}})
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := store.Load(ctx); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing object err = %v", err)
	}
	// Create-only.
	if err := store.Save(ctx, []byte("hosts: []\n"), ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, []byte("again"), ""); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("create over existing err = %v", err)
	}
	raw, etag, err := store.Load(ctx)
	if err != nil || string(raw) != "hosts: []\n" || etag == "" {
		t.Fatalf("Load = %q %q %v", raw, etag, err)
	}
	if o := fake.Object("cfg", "mrsh/hosts.yaml"); o.SSE != "AES256" {
		t.Errorf("config saved without SSE: %+v", o)
	}

	// Another machine writes; our stale ETag must be rejected.
	fake.Put("cfg", "mrsh/hosts.yaml", []byte("hosts:\n  - {name: other, host: h}\n"))
	if err := store.Save(ctx, []byte("mine"), etag); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale If-Match err = %v", err)
	}
	if o := fake.Object("cfg", "mrsh/hosts.yaml"); string(o.Data) == "mine" {
		t.Fatal("stale write clobbered the object")
	}

	// Reload and reapply succeeds.
	_, fresh, _ := store.Load(ctx)
	if err := store.Save(ctx, []byte("mine"), fresh); err != nil {
		t.Fatal(err)
	}
	var sawIfMatch bool
	for _, r := range fake.Requests() {
		if r.Op == "PutObject" && r.IfMatch == fresh {
			sawIfMatch = true
		}
	}
	if !sawIfMatch {
		t.Error("Save did not send If-Match")
	}
}

func TestS3StoreMutateReloadsOnConflict(t *testing.T) {
	fake := s3test.Start(t, "cfg")
	ctx := context.Background()
	fake.Put("cfg", "hosts.yaml", []byte("hosts:\n  - {name: a, host: h1}\n"))
	store, _ := NewStore("s3://cfg/hosts.yaml", StoreOptions{})

	first := true
	err := Mutate(ctx, store, func(d *Document, _ *Config) error {
		if first {
			// A concurrent writer lands between our Load and Save.
			first = false
			fake.Put("cfg", "hosts.yaml", []byte("hosts:\n  - {name: a, host: h1}\n  - {name: b, host: h2}\n"))
		}
		return d.AddHost(Host{Name: "c", Host: "h3"})
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(fake.Object("cfg", "hosts.yaml").Data)
	if err != nil || len(c.Hosts) != 3 {
		t.Fatalf("hosts after retry = %+v, %v (the concurrent write must survive)", c, err)
	}
}

func TestS3RegionDetection(t *testing.T) {
	fake := s3test.Start(t, "cfg")
	fake.Region = "ap-southeast-2"
	_ = os.Unsetenv("AWS_REGION") // t.Setenv in Start restores it
	fake.Put("cfg", "hosts.yaml", []byte("hosts: []\n"))
	store, _ := NewStore("s3://cfg/hosts.yaml", StoreOptions{})
	if _, _, err := store.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := store.(*S3Store).client.Options().Region; got != "ap-southeast-2" {
		t.Errorf("region = %q, want ap-southeast-2", got)
	}
}

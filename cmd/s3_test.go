package cmd

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/mbevc1/mrsh/internal/config"
	"github.com/mbevc1/mrsh/internal/s3test"
	"github.com/mbevc1/mrsh/internal/sshtest"
)

func TestS3ConfigHostsLifecycle(t *testing.T) {
	fake := s3test.Start(t, "cfg")
	uri := "s3://cfg/mrsh/hosts.yaml"

	if _, err := execRoot(t, "-f", uri, "--sse", "aws:kms", "--kms-key", "arn:aws:kms:eu-west-1:123456789012:key/k", "hosts", "init"); err != nil {
		t.Fatal(err)
	}
	if o := fake.Object("cfg", "mrsh/hosts.yaml"); o == nil || o.SSE != "aws:kms" || o.KMSKey == "" {
		t.Fatalf("init object = %+v", o)
	}
	if _, err := execRoot(t, "-f", uri, "hosts", "init"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("second init err = %v", err)
	}
	if _, err := execRoot(t, "-f", uri, "hosts", "add", "--name", "web01", "-H", "10.0.0.1", "-g", "web", "--pass", "s3cret"); err != nil {
		t.Fatal(err)
	}
	out, err := execRoot(t, "-f", uri, "hosts", "list")
	if err != nil || !strings.Contains(out, "web01") || strings.Contains(out, "s3cret") {
		t.Fatalf("list err=%v\n%s", err, out)
	}
	if _, err := execRoot(t, "-f", uri, "hosts", "update", "--name", "web01", "-g", "db"); err != nil {
		t.Fatal(err)
	}
	if _, err := execRoot(t, "-f", uri, "hosts", "remove", "--name", "web01", "--confirm"); err != nil {
		t.Fatal(err)
	}
	c, err := config.Parse(fake.Object("cfg", "mrsh/hosts.yaml").Data)
	if err != nil || len(c.Hosts) != 0 {
		t.Errorf("final config = %+v, %v", c, err)
	}
	// The starter comments survive S3 round trips too.
	if !strings.Contains(string(fake.Object("cfg", "mrsh/hosts.yaml").Data), "# mrsh hosts config") {
		t.Error("comments lost")
	}
}

func TestS3ConfigMissingAndRun(t *testing.T) {
	fake := s3test.Start(t, "cfg")
	if _, err := execRoot(t, "-f", "s3://cfg/nope.yaml", "hosts", "list"); err == nil || !strings.Contains(err.Error(), "hosts init") {
		t.Errorf("missing object err = %v", err)
	}

	srv := sshtest.Start(t, sshtest.Options{User: "deploy", Password: "pw"})
	fake.Put("cfg", "hosts.yaml", []byte(fmt.Sprintf("defaults: {user: deploy}\nhosts:\n  - {name: box, host: %s, port: %d, pass: pw}\n", srv.Host, srv.Port)))
	out, err := execRoot(t, "-f", "s3://cfg/hosts.yaml", "run", "-c", "echo via-s3")
	if err != nil || !strings.Contains(out, "via-s3") {
		t.Errorf("run err=%v\n%s", err, out)
	}
}

func TestS3MtBackup(t *testing.T) {
	f, cfg := startRouter(t)
	fake := s3test.Start(t, "backups")
	out, err := execRoot(t, "-f", cfg, "--sse", "AES256", "mt", "backup", "--path", "s3://backups/mrsh/")
	if err != nil {
		t.Fatalf("err=%v\n%s", err, out)
	}
	rsc := fake.Object("backups", "mrsh/lab/rt1/2026-04-06.rsc")
	bin := fake.Object("backups", "mrsh/lab/rt1/2026-04-06.backup")
	if rsc == nil || bin == nil || string(bin.Data) != "BINARYBACKUP" || rsc.SSE != "AES256" {
		t.Fatalf("objects: rsc=%+v bin=%+v", rsc, bin)
	}
	// The device keeps its rolling files.
	if _, err := os.Stat(f.root + "/backup-Mon.rsc"); err != nil {
		t.Error(err)
	}
}

func TestS3MtBackupMissingBucketTouchesNoDevice(t *testing.T) {
	f, cfg := startRouter(t)
	s3test.Start(t, "other")
	_, err := execRoot(t, "-f", cfg, "mt", "backup", "--path", "s3://missing/p/")
	if err == nil || exitCode(err) != exitUsage || !strings.Contains(err.Error(), "backup destination") {
		t.Errorf("err = %v", err)
	}
	if len(f.sent()) != 0 {
		t.Errorf("device commands ran: %v", f.sent())
	}
}

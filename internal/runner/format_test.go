package runner

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

var sample = []Result{
	{Name: "web01", Host: "10.0.0.1", Group: "web", Stdout: "up 4 days\nload 0.1\n", Duration: 142 * time.Millisecond},
	{Name: "db01", Host: "10.0.0.10", Group: "db", ExitCode: 1, Stderr: "permission \"denied\", sorry\n", Duration: 88 * time.Millisecond},
	{Name: "x", Host: "x", ExitCode: -1, Err: errors.New("dial x:22: refused")},
}

func TestWriteJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, FormatJSON, sample); err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	if len(got) != 3 {
		t.Fatalf("len = %d", len(got))
	}
	want := map[string]any{"host": "10.0.0.1", "name": "web01", "group": "web", "exit_code": 0.0,
		"stdout": "up 4 days\nload 0.1\n", "stderr": "", "duration_ms": 142.0}
	for k, v := range want {
		if got[0][k] != v {
			t.Errorf("%s = %v, want %v", k, got[0][k], v)
		}
	}
	if _, ok := got[0]["error"]; ok {
		t.Error("error key present on success")
	}
	if got[2]["error"] != "dial x:22: refused" || got[2]["exit_code"] != -1.0 {
		t.Errorf("error row = %v", got[2])
	}
	// Spec key order.
	if !strings.Contains(buf.String(), `"host": "10.0.0.1",
    "name": "web01",
    "group": "web",
    "exit_code": 0,
    "stdout"`) {
		t.Errorf("key order wrong:\n%s", buf.String())
	}
}

func TestWriteCSV(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, FormatCSV, sample); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "host,name,group,exit_code,stdout,stderr,duration_ms,error\n") {
		t.Errorf("header:\n%s", buf.String())
	}
	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("CSV does not parse back: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %d", len(rows))
	}
	if rows[1][4] != "up 4 days\nload 0.1\n" || rows[1][6] != "142" {
		t.Errorf("multi-line stdout row = %q", rows[1])
	}
	if rows[2][5] != "permission \"denied\", sorry\n" || rows[2][3] != "1" {
		t.Errorf("quoted stderr row = %q", rows[2])
	}
	if rows[3][7] != "dial x:22: refused" {
		t.Errorf("error row = %q", rows[3])
	}
}

func TestWriteUnknownFormat(t *testing.T) {
	if err := Write(&bytes.Buffer{}, "yaml", sample); err == nil {
		t.Error("unknown format accepted")
	}
}

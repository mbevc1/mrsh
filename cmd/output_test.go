package cmd

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

// captureStdout redirects os.Stdout to a buffer during fn, then restores it.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	old := os.Stdout
	os.Stdout = w

	fn()

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("io.Copy: %v", err)
	}
	return buf.String()
}

func sampleResults() []Result {
	return []Result{
		{
			Host:       "10.0.0.1",
			Name:       "web01",
			Group:      "web",
			ExitCode:   0,
			Stdout:     "up 4 days",
			Stderr:     "",
			DurationMs: 142,
		},
		{
			Host:       "10.0.0.2",
			Name:       "web02",
			Group:      "web",
			ExitCode:   1,
			Stdout:     "",
			Stderr:     "permission denied",
			DurationMs: 88,
		},
	}
}

func TestPrintJSON(t *testing.T) {
	results := sampleResults()
	out := captureStdout(t, func() {
		printJSON(results)
	})

	var parsed []Result
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v\nOutput: %s", err, out)
	}

	if len(parsed) != 2 {
		t.Errorf("expected 2 results in JSON, got %d", len(parsed))
	}
	if parsed[0].Host != "10.0.0.1" {
		t.Errorf("parsed[0].Host = %q, want 10.0.0.1", parsed[0].Host)
	}
	if parsed[0].DurationMs != 142 {
		t.Errorf("parsed[0].DurationMs = %d, want 142", parsed[0].DurationMs)
	}
	if parsed[1].ExitCode != 1 {
		t.Errorf("parsed[1].ExitCode = %d, want 1", parsed[1].ExitCode)
	}
}

func TestPrintCSV(t *testing.T) {
	results := sampleResults()
	out := captureStdout(t, func() {
		printCSV(results)
	})

	r := csv.NewReader(strings.NewReader(out))
	records, err := r.ReadAll()
	if err != nil {
		t.Fatalf("output is not valid CSV: %v\nOutput: %s", err, out)
	}

	// Header + 2 data rows.
	if len(records) != 3 {
		t.Fatalf("expected 3 CSV rows (header + 2 data), got %d", len(records))
	}

	header := records[0]
	wantHeader := []string{"host", "name", "group", "exit_code", "stdout", "stderr", "duration_ms"}
	for i, col := range wantHeader {
		if i >= len(header) || header[i] != col {
			t.Errorf("header[%d] = %q, want %q", i, header[i], col)
		}
	}

	// First data row.
	if records[1][0] != "10.0.0.1" {
		t.Errorf("row1 host = %q, want 10.0.0.1", records[1][0])
	}
	if records[1][3] != "0" {
		t.Errorf("row1 exit_code = %q, want 0", records[1][3])
	}

	// Second data row exit code.
	if records[2][3] != "1" {
		t.Errorf("row2 exit_code = %q, want 1", records[2][3])
	}
}

func TestPrintText(t *testing.T) {
	results := sampleResults()
	out := captureStdout(t, func() {
		printText(results)
	})

	if !strings.Contains(out, "10.0.0.1") {
		t.Errorf("text output missing host 10.0.0.1\nOutput: %s", out)
	}
	if !strings.Contains(out, "web01") {
		t.Errorf("text output missing name web01\nOutput: %s", out)
	}
	if !strings.Contains(out, "HOST") {
		t.Errorf("text output missing header row\nOutput: %s", out)
	}
}

func TestPrintResults_Formats(t *testing.T) {
	results := sampleResults()

	for _, format := range []string{"text", "json", "csv"} {
		t.Run(format, func(t *testing.T) {
			out := captureStdout(t, func() {
				printResults(results, format)
			})
			if len(out) == 0 {
				t.Errorf("printResults(%q) produced empty output", format)
			}
		})
	}
}

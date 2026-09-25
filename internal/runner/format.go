package runner

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// WriteText prints results as the default table. OUTPUT shows the first
// line of stdout, else of stderr (prefixed [stderr]), else the error.
func WriteText(w io.Writer, results []Result) error {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tHOST\tGROUP\tEXIT\tDURATION\tOUTPUT")
	for _, r := range results {
		group := r.Group
		if group == "" {
			group = "-"
		}
		exit := strconv.Itoa(r.ExitCode)
		if r.Err != nil && r.ExitCode == -1 {
			exit = "-"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Name, r.Host, group, exit, formatDuration(r.Duration), summary(r))
	}
	return tw.Flush()
}

func summary(r Result) string {
	switch {
	case strings.TrimSpace(r.Stdout) != "":
		return firstLine(r.Stdout)
	case strings.TrimSpace(r.Stderr) != "":
		return "[stderr] " + firstLine(r.Stderr)
	case r.Err != nil:
		return "[error] " + oneLine(r.Err.Error())
	}
	return ""
}

// firstLine returns the first non-empty line, noting how many more follow.
func firstLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		out := strings.TrimRight(l, "\r")
		if rest := len(lines) - i - 1; rest > 0 {
			out += fmt.Sprintf(" (+%d lines)", rest)
		}
		return out
	}
	return ""
}

func oneLine(s string) string { return strings.ReplaceAll(s, "\n", " ") }

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
	}
	return d.Round(10 * time.Millisecond).String()
}

// Output formats.
const (
	FormatText = "text"
	FormatJSON = "json"
	FormatCSV  = "csv"
)

// Write prints results in format.
func Write(w io.Writer, format string, results []Result) error {
	switch format {
	case FormatText, "":
		return WriteText(w, results)
	case FormatJSON:
		return WriteJSON(w, results)
	case FormatCSV:
		return WriteCSV(w, results)
	}
	return fmt.Errorf("unknown output format %q", format)
}

// record is the JSON/CSV shape of a Result. Field order is the column order.
type record struct {
	Host       string `json:"host"`
	Name       string `json:"name"`
	Group      string `json:"group"`
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

func toRecord(r Result) record {
	rec := record{
		Host: r.Host, Name: r.Name, Group: r.Group, ExitCode: r.ExitCode,
		Stdout: r.Stdout, Stderr: r.Stderr, DurationMS: r.Duration.Milliseconds(),
	}
	if r.Err != nil {
		rec.Error = r.Err.Error()
	}
	return rec
}

// WriteJSON prints results as a JSON array.
func WriteJSON(w io.Writer, results []Result) error {
	recs := make([]record, len(results))
	for i, r := range results {
		recs[i] = toRecord(r)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(recs)
}

var csvHeader = []string{"host", "name", "group", "exit_code", "stdout", "stderr", "duration_ms", "error"}

// WriteCSV prints results as RFC 4180 CSV; multi-line output is quoted.
func WriteCSV(w io.Writer, results []Result) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(csvHeader); err != nil {
		return err
	}
	for _, r := range results {
		rec := toRecord(r)
		if err := cw.Write([]string{
			rec.Host, rec.Name, rec.Group, strconv.Itoa(rec.ExitCode), rec.Stdout, rec.Stderr,
			strconv.FormatInt(rec.DurationMS, 10), rec.Error,
		}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

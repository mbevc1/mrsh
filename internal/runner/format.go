package runner

import (
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

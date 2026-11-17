package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mbevc1/mrsh/internal/config"
)

// errDryRun ends a command whose --dry-run preview has been printed. The
// root command turns it into success, so no "updated ..." line follows.
var errDryRun = errors.New("dry run: nothing written")

// dryRunStore is the config store under --dry-run: it reads as usual, and
// Save prints what would change instead of writing. Everything that edits
// the config goes through openStore, so no command can write by accident.
type dryRunStore struct {
	config.ConfigStore
	out io.Writer
}

func (s dryRunStore) Save(ctx context.Context, raw []byte, ifVersion string) error {
	old, version, err := s.Load(ctx)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		old = nil
	case err != nil:
		return err
	case ifVersion == "" || ifVersion != version:
		// The real store would refuse too: create-only, or changed since read.
		return config.ErrVersionConflict
	}
	loc := s.Location()
	if string(old) == string(raw) {
		_, err = fmt.Fprintf(s.out, "dry-run: %s would not change\n", loc)
	} else {
		_, err = fmt.Fprintf(s.out, "dry-run: would write %s (nothing written):\n%s", loc, configDiff(loc, old, raw))
	}
	if err != nil {
		return err
	}
	return errDryRun
}

// passValue finds a literal password in a YAML line, in block or flow
// style: the key "pass" (not pass_env or pass_arn) and its scalar value.
var passValue = regexp.MustCompile(`(\bpass:[ \t]*)("(?:[^"\\]|\\.)*"|'(?:[^']|'')*'|[^,}#\s](?:[^,}#\n]*[^,}#\s])?)`)

// maskPasswords replaces literal password values in both texts with
// <password #N>, numbered per distinct value, so a diff shows that a
// password changed without showing it.
func maskPasswords(texts ...[]byte) []string {
	ids := map[string]int{}
	out := make([]string, len(texts))
	for i, t := range texts {
		out[i] = passValue.ReplaceAllStringFunc(string(t), func(m string) string {
			sub := passValue.FindStringSubmatch(m)
			if _, ok := ids[sub[2]]; !ok {
				ids[sub[2]] = len(ids) + 1
			}
			return fmt.Sprintf("%s<password #%d>", sub[1], ids[sub[2]])
		})
	}
	return out
}

// configDiff is a unified diff of a config before and after an edit, with
// literal passwords masked.
func configDiff(name string, before, after []byte) string {
	masked := maskPasswords(before, after)
	return unifiedDiff(name, splitLines(masked[0]), splitLines(masked[1]))
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// diffContext is how many unchanged lines surround each change.
const diffContext = 3

// unifiedDiff compares a and b line by line (longest common subsequence;
// configs are small) and formats the changes as unified-diff hunks.
func unifiedDiff(name string, a, b []string) string {
	type op struct {
		kind byte // ' ', '-' or '+'
		text string
	}
	// lcs[i][j] is the common length of a[i:] and b[j:].
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var ops []op
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			ops = append(ops, op{' ', a[i]})
			i, j = i+1, j+1
		case i < len(a) && (j == len(b) || lcs[i+1][j] >= lcs[i][j+1]):
			ops = append(ops, op{'-', a[i]}) // removals before additions, as diff(1) shows them
			i++
		default:
			ops = append(ops, op{'+', b[j]})
			j++
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s (after)\n", name, name)
	for start := 0; start < len(ops); {
		if ops[start].kind == ' ' {
			start++
			continue
		}
		// A hunk runs from diffContext lines before this change to
		// diffContext lines after the last change within reach.
		from := max(start-diffContext, 0)
		end := start
		for k := start; k < len(ops) && k <= end+2*diffContext; k++ {
			if ops[k].kind != ' ' {
				end = k
			}
		}
		to := min(end+diffContext+1, len(ops))
		aLine, bLine := 1, 1
		for _, o := range ops[:from] {
			if o.kind != '+' {
				aLine++
			}
			if o.kind != '-' {
				bLine++
			}
		}
		aCount, bCount := 0, 0
		for _, o := range ops[from:to] {
			if o.kind != '+' {
				aCount++
			}
			if o.kind != '-' {
				bCount++
			}
		}
		fmt.Fprintf(&sb, "@@ -%s +%s @@\n", hunkRange(aLine, aCount), hunkRange(bLine, bCount))
		for _, o := range ops[from:to] {
			fmt.Fprintf(&sb, "%c%s\n", o.kind, o.text)
		}
		start = to
	}
	return sb.String()
}

// hunkRange is a unified-diff range: an empty range names the line before it.
func hunkRange(line, count int) string {
	if count == 0 {
		return fmt.Sprintf("%d,0", line-1)
	}
	return fmt.Sprintf("%d,%d", line, count)
}

// dryRunSecrets lists the SSM parameters a removal would delete.
func dryRunSecrets(cmd *cobra.Command, cfg *config.Config, del, kept []string) {
	out := cmd.OutOrStdout()
	for _, arn := range del {
		name, _ := cfg.ManagedSSMName(arn)
		_, _ = fmt.Fprintf(out, "dry-run: would delete SSM parameter %s\n", name)
	}
	for _, k := range kept {
		_, _ = fmt.Fprintln(out, "dry-run: "+k)
	}
}

// dryRunOK turns the dry-run ending into success for every command.
func dryRunOK(root *cobra.Command) {
	for _, c := range root.Commands() {
		if run := c.RunE; run != nil {
			c.RunE = func(cmd *cobra.Command, args []string) error {
				if err := run(cmd, args); !errors.Is(err, errDryRun) {
					return err
				}
				return nil
			}
		}
		dryRunOK(c)
	}
}

package cmd

import (
	"encoding/json"
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"
)

// Build-time metadata, overwritten via
// -ldflags "-X github.com/mbevc1/mrsh/cmd.version=...".
var (
	appName = "mrsh"
	version = "dev"
	commit  = ""
	date    = ""
	builtBy = ""
)

// readBuildInfo is a variable so tests can supply fake build info.
var readBuildInfo = debug.ReadBuildInfo

// versionInfo is the `mrsh version --output json` shape.
type versionInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	BuiltBy string `json:"builtBy"`
	Go      string `json:"go"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

// resolveVersion returns the version, revision and build time in priority
// order: ldflags value, then module version (go install), then "dev".
// Commit and date fall back to the VCS stamps Go embeds in module builds.
func resolveVersion() (ver, rev, when string) {
	ver, rev, when = version, commit, date
	bi, ok := readBuildInfo()
	if !ok {
		return
	}
	fromLDFlags := ver != "dev"
	if !fromLDFlags && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		ver = bi.Main.Version
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if rev == "" {
				rev = s.Value
			}
		case "vcs.time":
			if when == "" {
				when = s.Value
			}
		case "vcs.modified":
			// ldflags versions (git describe --dirty) and Go 1.24+ VCS
			// pseudo-versions ("+dirty") may already carry the marker.
			if s.Value == "true" && !fromLDFlags && !strings.Contains(ver, "dirty") {
				ver += "-dirty"
			}
		}
	}
	return
}

func currentVersionInfo() versionInfo {
	ver, rev, when := resolveVersion()
	return versionInfo{
		Name:    appName,
		Version: ver,
		Commit:  rev,
		Date:    when,
		BuiltBy: builtBy,
		Go:      runtime.Version(),
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
	}
}

func newVersionCmd(opts *globalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show mrsh build info",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := currentVersionInfo()
			out := cmd.OutOrStdout()
			switch opts.output {
			case "json":
				return json.NewEncoder(out).Encode(info)
			case "text":
				_, err := fmt.Fprintf(out,
					"%s %s\n  commit:  %s\n  built:   %s\n  by:      %s\n  go:      %s\n  os/arch: %s/%s\n",
					info.Name, info.Version, orUnknown(info.Commit), orUnknown(info.Date),
					orUnknown(info.BuiltBy), info.Go, info.OS, info.Arch)
				return err
			default:
				return fmt.Errorf("version does not support --output %s", opts.output)
			}
		},
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

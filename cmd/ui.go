package cmd

import (
	"fmt"
	"log/slog"
	"os/exec"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/mbevc1/mrsh/internal/webui"
)

// openBrowser opens url in the desktop browser; tests replace it.
var openBrowser = func(url string) error {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", url)
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		c = exec.Command("xdg-open", url)
	}
	return c.Start()
}

func newUICmd(opts *globalOptions) *cobra.Command {
	var addr string
	var noOpen bool
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Edit the hosts config in a local browser UI",
		Long: "Starts a loopback-only web editor for the --config source (local or s3://) and opens it.\n" +
			"It edits config only: it never resolves secrets, connects to hosts or runs commands.\n" +
			"Stop it with Ctrl-C or the page's Exit button; nothing keeps running afterwards.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := openStore(opts)
			if err != nil {
				return err
			}
			// Fail early on a missing or broken config.
			if _, err := loadConfig(cmd, opts); err != nil {
				return err
			}
			srv, ln, err := webui.Listen(store, addr)
			if err != nil {
				return err
			}
			url := srv.URL()
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "mrsh ui editing %s\nopen %s\npress Ctrl-C or the Exit button to stop\n", store.Location(), url)
			if !noOpen {
				if err := openBrowser(url); err != nil {
					slog.Warn("could not open a browser; open the URL above yourself", "err", err)
				}
			}
			if err := srv.Serve(cmd.Context(), ln); err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "mrsh ui stopped")
			return err
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:7171", "listen address (loopback only)")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "don't open the browser")
	return cmd
}

package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print mrsh version and build information",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("%s version %s\n", Name, Version)
		if Commit != "" {
			fmt.Printf("  commit:   %s\n", Commit)
		}
		if Date != "" {
			fmt.Printf("  date:     %s\n", Date)
		}
		if BuiltBy != "" {
			fmt.Printf("  built by: %s\n", BuiltBy)
		}
	},
}

// Name is the application name, set from main.
var Name = "mrsh"

func init() {
	rootCmd.AddCommand(versionCmd)
}

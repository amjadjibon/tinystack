package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Execute runs the root command and exits with a nonzero status on failure.
func Execute() {
	if err := newRootCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "tinystack",
		Short:         "A lightweight local AWS emulator",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       "0.1.0-dev",
	}

	root.AddCommand(newStartCommand())
	root.AddCommand(newLoginCommand())
	root.AddCommand(newStateCommand())
	root.AddCommand(newCACommand())
	return root
}

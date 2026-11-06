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
		Version:       versionString(),
	}

	root.AddCommand(newStartCommand())
	root.AddCommand(newLoginCommand())
	root.AddCommand(newStateCommand())
	root.AddCommand(newCACommand())
	root.AddCommand(newVersionCommand())
	root.AddCommand(newUpdateCommand())
	return root
}

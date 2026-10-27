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

// version is replaced at release time with -ldflags "-X github.com/amjadjibon/tinystack/internal/cmd.version=1.2.3".
var version = "0.1.0-dev"

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "tinystack",
		Short:         "A lightweight local AWS emulator",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
	}

	root.AddCommand(newStartCommand())
	root.AddCommand(newLoginCommand())
	root.AddCommand(newStateCommand())
	root.AddCommand(newCACommand())
	return root
}

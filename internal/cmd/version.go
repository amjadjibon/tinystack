package cmd

import (
	"fmt"
	"regexp"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// version and commit are set at build time from git, e.g. by `task build`:
//
//	-ldflags "-X github.com/amjadjibon/tinystack/internal/cmd.version=0.0.1
//	          -X github.com/amjadjibon/tinystack/internal/cmd.commit=1a2b3c4d5e6f"
//
// version is the tag without its v, or `git describe` output (0.0.1-3-g1a2b3c4d5e6f-dirty) between tags.
var (
	version = "dev"
	commit  = ""
)

// developmentVersion matches unstamped builds and `git describe` output for commits after a tag or with local edits.
var developmentVersion = regexp.MustCompile(`^dev$|-\d+-g[0-9a-f]+(-dirty)?$|-dirty$`)

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use: "version", Short: "Print the version and commit", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "tinystack version %s\n", versionString())
			return err
		},
	}
}

// versionString is the version followed by the commit, which falls back to the VCS stamp `go build` embeds.
func versionString() string {
	if c := buildCommit(); c != "" {
		return fmt.Sprintf("%s (commit %s)", version, c)
	}

	return version
}

func buildCommit() string {
	if commit != "" {
		return commit
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}

	var revision, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}

	if len(revision) > 12 {
		revision = revision[:12]
	}

	if revision != "" && modified == "true" {
		revision += "-dirty"
	}

	return revision
}

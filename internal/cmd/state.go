package cmd

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/amjadjibon/tinystack/internal/storage/archive"
)

func newStateCommand() *cobra.Command {
	state := &cobra.Command{
		Use:   "state",
		Short: "Manage TinyStack persistent state",
	}

	state.AddCommand(newExportCommand())
	state.AddCommand(newImportCommand())
	return state
}

func newExportCommand() *cobra.Command {
	var (
		storageDir string
		outputFile string
		overwrite  bool
	)

	export := &cobra.Command{
		Use:   "export",
		Short: "Export persistent state to a portable fixture archive",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if storageDir == "" {
				return errors.New("storage directory is required (--storage-dir)")
			}

			if outputFile == "" {
				return errors.New("output file is required (--output)")
			}

			opts := archive.ExportOptions{
				StorageDir: storageDir,
				OutputFile: outputFile,
				Overwrite:  overwrite,
			}
			return archive.Export(cmd.Context(), opts)
		},
	}

	export.Flags().StringVar(
		&storageDir,
		"storage-dir",
		"",
		"path to the persistent storage directory (contains state.db)",
	)
	export.Flags().StringVar(&outputFile, "output", "", "path to the output fixture archive file")
	export.Flags().BoolVar(&overwrite, "overwrite", false, "overwrite existing destination archive if it exists")

	return export
}

func newImportCommand() *cobra.Command {
	var (
		storageDir string
		inputFile  string
	)

	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import an offline state fixture archive into a new persistent storage directory",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if inputFile == "" {
				return errors.New("input file is required (--input)")
			}

			if storageDir == "" {
				return errors.New("storage directory is required (--storage-dir)")
			}

			opts := archive.ImportOptions{
				InputFile:  inputFile,
				StorageDir: storageDir,
			}
			return archive.Import(cmd.Context(), opts)
		},
	}

	cmd.Flags().StringVar(&inputFile, "input", "", "path to the input fixture archive file (.tar.gz or .tar)")
	cmd.Flags().StringVar(
		&storageDir,
		"storage-dir",
		"",
		"path to the new persistent storage directory to create",
	)

	return cmd
}

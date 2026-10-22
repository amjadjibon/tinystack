package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/localtls"
)

func newCACommand() *cobra.Command {
	var configPath, out string
	ca := &cobra.Command{Use: "ca", Short: "Manage the local HTTPS certificate authority"}
	export := &cobra.Command{
		Use: "export", Short: "Export the local CA certificate, creating it if absent", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}

			if _, err = config.ApplyLocalStackEnv(&cfg, nil, nil); err != nil {
				return err
			}

			if guardErr := cfg.Validate(); guardErr != nil {
				return guardErr
			}

			if cfg.TLS.CertFile != "" {
				return fmt.Errorf("CA export is unavailable with tls.cert_file; use your issuer's CA")
			}

			dir, err := cfg.DataDir()
			if err != nil {
				return err
			}

			authority, err := localtls.Load(dir)
			if err != nil {
				return err
			}

			if out != "" {
				return os.WriteFile(out, authority.PEM, 0o644)
			}

			_, err = cmd.OutOrStdout().Write(authority.PEM)
			return err
		},
	}

	export.Flags().StringVar(&configPath, "config", "", "YAML configuration path (same as start)")
	export.Flags().StringVar(&out, "out", "", "output PEM file (default stdout)")
	ca.AddCommand(export)
	return ca
}

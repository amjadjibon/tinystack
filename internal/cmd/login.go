package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/sso"
)

// reuseFor is how long before expiry cached credentials stop being reused.
const reuseFor = 5 * time.Minute

var profileName = regexp.MustCompile(`^[\w.-]{1,64}$`)

func newLoginCommand() *cobra.Command {
	var configPath, endpoint, profile string
	login := &cobra.Command{
		Use:   "login",
		Short: "Sign in through the console's OpenID Connect provider and print AWS credentials",
		Long: "Signs in with the OAuth device authorization flow, using the console.auth.oidc settings of the " +
			"configuration, and prints the credentials of your role as the JSON that the AWS CLI's " +
			"credential_process setting reads. Credentials are cached per profile until shortly before they expire.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !profileName.MatchString(profile) {
				return errors.New("--profile must be a name of letters, digits, dots, dashes and underscores")
			}

			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}

			oidc := cfg.Console.Auth.OIDC
			if oidc == nil {
				return errors.New("the configuration has no console.auth.oidc")
			}

			if endpoint == "" {
				host := cfg.Server.Host
				if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
					host = "127.0.0.1"
				}

				endpoint = "http://" + net.JoinHostPort(host, strconv.Itoa(cfg.Server.ConsolePort))
			}

			cache, err := cachePath(profile)
			if err != nil {
				return err
			}

			process := cached(cache)
			if process == nil {
				if process, err = sso.Login(cmd.Context(), oidc, endpoint, cmd.ErrOrStderr()); err != nil {
					return err
				}

				err = store(cache, process)
				if err != nil {
					return err
				}
			}

			// #nosec G117 -- the credentials are the command's output, for credential_process.
			return json.NewEncoder(cmd.OutOrStdout()).Encode(process)
		},
	}

	login.Flags().StringVar(&profile, "profile", "", "name for the cached credentials (required)")
	login.Flags().StringVar(&configPath, "config", "", "the YAML configuration that TinyStack runs with")
	login.Flags().
		StringVar(&endpoint, "endpoint", "", "the console URL (default: from server.host and server.console_port)")
	_ = login.MarkFlagRequired("profile")
	_ = login.MarkFlagRequired("config")
	return login
}

func cachePath(profile string) (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(dir, "tinystack", "sso", profile+".json"), nil
}

// cached returns the profile's credentials while they have time left.
func cached(path string) *sso.Process {
	// #nosec G304 -- the path is the user's cache directory and a validated profile name.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var process sso.Process
	if json.Unmarshal(data, &process) != nil || process.AccessKeyID == "" {
		return nil
	}

	if expires, err := time.Parse(time.RFC3339, process.Expiration); err != nil || time.Until(expires) < reuseFor {
		return nil
	}

	return &process
}

// store keeps credentials readable by the user alone.
func store(path string, process *sso.Process) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	// #nosec G117 -- the credentials are cached for the user alone, with mode 0600.
	data, err := json.Marshal(process)
	if err != nil {
		return err
	}

	if err = os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("cache the credentials: %w", err)
	}

	return nil
}

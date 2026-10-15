package cmd

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/spf13/cobra"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/sso"
)

// reuseFor is how long before expiry cached credentials stop being reused.
const reuseFor = 5 * time.Minute

var profileName = regexp.MustCompile(`^[\w.-]{1,64}$`)

func newLoginCommand() *cobra.Command {
	var configPath, endpoint, profile, role string
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
				public, parseErr := url.Parse(oidc.RedirectURL)
				if parseErr != nil {
					return parseErr
				}

				endpoint = public.Scheme + "://" + public.Host
			}

			cacheProfile := profile
			if role != "" {
				cacheProfile += fmt.Sprintf("-%x", sha256.Sum256([]byte(role)))
			}

			cache, err := cachePath(cacheProfile)
			if err != nil {
				return err
			}

			process := cached(cache)
			if process == nil {
				if process, err = sso.Login(cmd.Context(), oidc, endpoint, role, cmd.ErrOrStderr()); err != nil {
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

	login.Flags().StringVar(&role, "role", "", "allowed role ARN (required when several roles match)")
	login.Flags().StringVar(&profile, "profile", "", "name for the cached credentials (required)")
	login.Flags().StringVar(&configPath, "config", "", "the YAML configuration that TinyStack runs with")
	login.Flags().
		StringVar(&endpoint, "endpoint", "", "the console URL (default: the redirect_url origin)")
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

	// Create a private new inode and atomically replace the cache. WriteFile
	// preserves an existing file's permissions and follows symlinks.
	file, err := os.CreateTemp(filepath.Dir(path), ".credentials-*")
	if err != nil {
		return fmt.Errorf("cache the credentials: %w", err)
	}

	defer func() { _ = os.Remove(file.Name()) }()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}

	if err := file.Close(); err != nil {
		return err
	}

	return os.Rename(file.Name(), path)
}

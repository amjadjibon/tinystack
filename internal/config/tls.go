package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/localtls"
)

func (c *Config) validateTLS() error {
	if (c.TLS.CertFile == "") != (c.TLS.KeyFile == "") {
		return fmt.Errorf("tls.cert_file and tls.key_file must be supplied together")
	}

	if c.TLS.CertFile != "" && len(c.TLS.Names) != 0 {
		return fmt.Errorf("tls.names requires generated certificates")
	}

	if err := localtls.ValidateNames(c.TLS.Names); err != nil {
		return err
	}

	host, port, err := net.SplitHostPort(c.TLS.Address)
	if err != nil {
		return fmt.Errorf("tls.address must be host:port: %w", err)
	}

	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 || host == "" {
		return fmt.Errorf("tls.address requires a host and numeric port between 0 and 65535")
	}

	return nil
}

// DataDir resolves storage.path (including ~/), or the per-user config directory
// for memory mode. Certificates persist independently of ephemeral AWS state.
func (c *Config) DataDir() (string, error) {
	dir := c.Storage.Path
	if c.Storage.Mode == StorageMemory {
		base, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}

		return filepath.Join(base, "tinystack"), nil
	}

	if dir == "~" || strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}

		dir = filepath.Join(home, dir[1:])
	}

	return dir, nil
}

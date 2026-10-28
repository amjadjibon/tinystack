package valkeydocker

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/redis/go-redis/v9"

	"github.com/amjadjibon/tinystack/internal/localtls"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
)

// ClusterSpec describes a persistent, single-node cluster.
type ClusterSpec struct {
	Name, Volume, ARN, Image, Admin string
	Port                            int64
	TLS                             bool
}

// ClusterRuntime manages persistent cache containers using the shared node runtime.
type ClusterRuntime struct {
	nodes  *Runtime
	ca     *localtls.Authority
	dirs   map[string]string
	label  string
	mu     sync.Mutex
	closed bool
}

// NewCluster constructs a runtime isolated to one store's ownership label.
func NewCluster(ca *localtls.Authority, owner string) *ClusterRuntime {
	return &ClusterRuntime{nodes: New(), ca: ca, label: "tinystack.memorydb." + owner, dirs: map[string]string{}}
}

// Resources lists this store's containers and retained volumes.
func (r *ClusterRuntime) Resources(ctx context.Context) ([]*ClusterSpec, error) {
	containers, err := dockercli.ListLabeled(ctx, r.label)
	if err != nil {
		return nil, err
	}

	volumes, err := dockercli.ListLabeledVolumes(ctx, r.label)
	if err != nil {
		return nil, err
	}

	var out []*ClusterSpec
	for id, arn := range containers {
		out = append(out, &ClusterSpec{Name: id, ARN: arn})
	}

	for name, arn := range volumes {
		out = append(out, &ClusterSpec{Volume: name, ARN: arn})
	}

	return out, nil
}

// ClusterArgs enables cluster protocol, durable writes and reachable announced endpoints.
func ClusterArgs(s *ClusterSpec) []string {
	args := []string{
		"--cluster-enabled", "yes", "--cluster-config-file", "/data/nodes.conf",
		"--cluster-announce-ip", "127.0.0.1", "--cluster-announce-port", fmt.Sprint(s.Port),
		"--appendonly", "yes", "--appendfsync", "always", "--protected-mode", "no", "--aclfile", "/tinystack/users.acl",
	}

	if s.TLS {
		args = append(args, "--port", "0", "--tls-port", "6379", "--tls-cert-file", "/tinystack/cert.pem",
			"--tls-key-file", "/tinystack/key.pem", "--tls-ca-cert-file", "/tinystack/ca.pem",
			"--tls-auth-clients", "no", "--tls-cluster", "yes", "--cluster-announce-tls-port", fmt.Sprint(s.Port))
	}

	return args
}

func (r *ClusterRuntime) configuration(s *ClusterSpec) (string, error) {
	privateDir, err := os.MkdirTemp("", "tinystack-memorydb-")
	if err != nil {
		return "", err
	}

	dir := filepath.Join(privateDir, "config")
	if err = os.Mkdir(dir, 0o755); err != nil {
		_ = os.RemoveAll(privateDir)
		return "", err
	}

	if err = os.Chmod(dir, 0o755); err != nil {
		_ = os.RemoveAll(privateDir)
		return "", err
	}

	// Only hashes of the internal management password go into engine configuration.
	hash := sha256.Sum256([]byte(s.Admin))
	files := map[string][]byte{"users.acl": fmt.Appendf(nil,
		"user default off\nuser __tinystack-admin on #%x ~* &* +@all\n", hash)}
	if s.TLS {
		config, configErr := r.ca.Config(nil)
		if configErr != nil {
			_ = os.RemoveAll(privateDir)
			return "", configErr
		}

		cert, certErr := config.GetCertificate(&tls.ClientHelloInfo{})
		if certErr != nil {
			_ = os.RemoveAll(privateDir)
			return "", certErr
		}

		key, keyErr := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
		if keyErr != nil {
			_ = os.RemoveAll(privateDir)
			return "", keyErr
		}

		files["key.pem"] = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})
		for _, der := range cert.Certificate {
			files["cert.pem"] = append(
				files["cert.pem"],
				pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
		}

		files["ca.pem"] = r.ca.PEM
	}

	for name, data := range files {
		// #nosec G306 -- the enclosing 0700 directory protects host access; the engine reads copied files.
		if err = os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			_ = os.RemoveAll(privateDir)
			return "", err
		}

		if err = os.Chmod(filepath.Join(dir, name), 0o644); err != nil {
			_ = os.RemoveAll(privateDir)
			return "", err
		}
	}

	return dir, nil
}

// Ensure reattaches a container or recreates it against the retained volume.
func (r *ClusterRuntime) Ensure(ctx context.Context, s *ClusterSpec) (Started, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return Started{}, fmt.Errorf("runtime is closed")
	}

	id, err := dockercli.Run(ctx, nil, "inspect", "--format", "{{.Id}}", s.Name)
	if err == nil && r.dirs[s.Name] == "" {
		// Refresh prior-process containers' local-CA leaves while preserving the data volume.
		r.nodes.Remove(ctx, id)
		err = ErrGone
	}

	if err == nil {
		r.nodes.mu.Lock()
		r.nodes.started[id] = true
		r.nodes.mu.Unlock()

		if _, err = dockercli.Run(ctx, nil, "start", s.Name); err != nil {
			return Started{}, err
		}

		port, portErr := dockercli.HostPort(ctx, s.Name, 6379)
		return Started{ID: id, Port: port}, portErr
	}

	if !errors.Is(err, ErrGone) && !dockercli.Gone(err) {
		return Started{}, err
	}

	// Choose the host port before creating the engine so cluster discovery announces it.
	listenConfig := net.ListenConfig{}
	listener, err := listenConfig.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return Started{}, err
	}

	_, port, err := net.SplitHostPort(listener.Addr().String())
	_ = listener.Close()
	if err != nil {
		return Started{}, err
	}

	s.Port, err = strconv.ParseInt(port, 10, 64)
	if err != nil {
		return Started{}, err
	}

	dir, err := r.configuration(s)
	if err != nil {
		return Started{}, err
	}

	if old := r.dirs[s.Name]; old != "" {
		_ = os.RemoveAll(old)
	}

	r.dirs[s.Name] = filepath.Dir(dir)
	return r.nodes.Start(ctx, &NodeSpec{
		Name:      s.Name,
		Volume:    s.Volume,
		Image:     s.Image,
		Port:      6379,
		HostPort:  s.Port,
		ConfigDir: dir,
		Args:      ClusterArgs(s),
		Labels:    map[string]string{r.label: s.ARN, "tinystack.memorydb": s.ARN},
	})
}

// Command executes an engine management command over the published endpoint.
func (r *ClusterRuntime) Command(ctx context.Context, s *ClusterSpec, args ...any) (string, error) {
	options := &redis.Options{
		Addr:     fmt.Sprintf("127.0.0.1:%d", s.Port),
		Username: "__tinystack-admin",
		Password: s.Admin,
	}

	if s.TLS {
		roots := x509.NewCertPool()
		roots.AppendCertsFromPEM(r.ca.PEM)
		options.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	}

	client := redis.NewClient(options)
	defer func() { _ = client.Close() }()
	result, err := client.Do(ctx, args...).Result()
	if err != nil {
		return "", err
	}

	if rows, ok := result.([]any); ok {
		var lines []string
		for _, row := range rows {
			lines = append(lines, fmt.Sprint(row))
		}

		return strings.Join(lines, "\n"), nil
	}

	return fmt.Sprint(result), nil
}

// Remove removes a cluster and optionally its durable volume.
func (r *ClusterRuntime) Remove(ctx context.Context, s *ClusterSpec, volume bool) error {
	if s.Name != "" {
		r.nodes.Remove(ctx, s.Name)
	}

	if volume && s.Volume != "" {
		if _, err := dockercli.Run(ctx, nil, "volume", "rm", s.Volume); err != nil && !dockercli.Gone(err) {
			return err
		}
	}

	return nil
}

// Close removes containers and temporary leaf certificates, keeping data volumes.
func (r *ClusterRuntime) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}

	r.closed = true

	r.nodes.Close()
	// Also stop owned containers left by interrupted starts, which may not have returned their IDs.
	ctx, cancel := context.WithTimeout(context.Background(), removeTimeout)
	defer cancel()
	containers, err := dockercli.ListLabeled(ctx, r.label)
	if err == nil {
		for id := range containers {
			r.nodes.Remove(ctx, id)
		}
	}

	for _, dir := range r.dirs {
		_ = os.RemoveAll(dir)
	}
}

// ACLCommand returns a full replacement ACL SETUSER command, using password hashes.
func ACLCommand(name, access string, hashes []string) []any {
	args := []any{"ACL", "SETUSER", name, "reset"}
	for _, rule := range strings.Fields(access) {
		args = append(args, rule)
	}

	for _, hash := range hashes {
		args = append(args, "#"+hash)
	}

	return args
}

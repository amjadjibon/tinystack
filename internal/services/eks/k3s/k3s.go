// Package k3s runs EKS clusters' Kubernetes API servers as k3s containers
// through the docker CLI.
//
// Each cluster is one privileged rancher/k3s server container (which is also
// the cluster's only node) with its API port published on 127.0.0.1. Clients
// do not reach that port: each cluster's endpoint is an HTTPS listener on
// 127.0.0.1, with a certificate from the runtime's own certificate authority,
// that accepts the tokens "aws eks get-token" makes and forwards requests as
// the cluster's administrator (whose credentials come from the kubeconfig k3s
// writes). Removing a cluster removes its container's volumes, so a cluster's
// Kubernetes state lives as long as its container.
package k3s

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	"github.com/amjadjibon/tinystack/internal/services/eks"
)

const (
	// readyTimeout bounds how long a started server may take to be ready.
	readyTimeout = 3 * time.Minute
	// removeTimeout bounds removing a container, which outlives its caller.
	removeTimeout = time.Minute
	kubeconfig    = "/etc/rancher/k3s/k3s.yaml"
)

// images are the k3s images that run each Kubernetes version EKS offers.
var images = map[string]string{
	"1.30": "rancher/k3s:v1.30.14-k3s1",
	"1.31": "rancher/k3s:v1.31.14-k3s1",
	"1.32": "rancher/k3s:v1.32.13-k3s1",
	"1.33": "rancher/k3s:v1.33.13-k3s1",
	"1.34": "rancher/k3s:v1.34.12-k3s1",
	"1.35": "rancher/k3s:v1.35.9-k3s1",
}

// Authenticate returns the account of the access key that signed a request's
// EKS token for a cluster (gateway.Server.KubernetesAccount).
type Authenticate func(r *http.Request, cluster string) (string, error)

var _ eks.Runtime = (*Runtime)(nil)

// Runtime runs API servers in k3s containers.
type Runtime struct {
	authenticate Authenticate
	logger       *slog.Logger
	ca           *x509.Certificate
	caKey        *ecdsa.PrivateKey
	// endpoints holds the endpoint of each container not yet removed; its
	// server is nil until the container is ready.
	endpoints map[string]*http.Server
	caPEM     string
	mu        sync.Mutex
	closed    bool
}

// New returns a runtime whose endpoints authenticate requests through
// authenticate.
func New(authenticate Authenticate, logger *slog.Logger) (*Runtime, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "TinyStack EKS"}, NotBefore: now.Add(-time.Hour),
		NotAfter: now.AddDate(10, 0, 0), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}

	ca, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}

	return &Runtime{
		authenticate: authenticate, logger: logger, ca: ca, caKey: key, endpoints: map[string]*http.Server{},
		caPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
	}, nil
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	return n
}

func (r *Runtime) Start(ctx context.Context, cluster eks.Cluster) (eks.Server, error) {
	image, ok := images[cluster.Version]
	if !ok {
		return eks.Server{}, fmt.Errorf("no k3s image runs Kubernetes %s", cluster.Version)
	}

	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return eks.Server{}, errors.New("runtime is closed")
	}

	id, err := dockercli.Run(ctx, nil, "run", "-d", "--privileged", "--label", "tinystack.eks.cluster="+cluster.Name,
		"--tmpfs", "/run", "--tmpfs", "/var/run", "-p", "127.0.0.1::6443", image, "server",
		"--disable=traefik", "--disable=metrics-server", "--tls-san=127.0.0.1", "--write-kubeconfig-mode=600")
	if err != nil {
		return eks.Server{}, err
	}

	// A container started while the runtime closed is removed at once.
	r.mu.Lock()
	closed = r.closed
	r.endpoints[id] = nil
	r.mu.Unlock()
	if closed {
		r.Remove(ctx, id)
		return eks.Server{}, errors.New("runtime is closed")
	}

	endpoint, err := r.serve(ctx, id, cluster)
	if err != nil {
		r.Remove(ctx, id)
		return eks.Server{}, err
	}

	return eks.Server{ID: id, URL: endpoint, CA: r.caPEM}, nil
}

// serve waits until a container's API server is ready and opens the cluster's
// endpoint to it, returning the endpoint's URL.
func (r *Runtime) serve(ctx context.Context, id string, cluster eks.Cluster) (string, error) {
	client, target, err := ready(ctx, id)
	if err != nil {
		return "", err
	}

	cert, err := r.certificate()
	if err != nil {
		return "", err
	}

	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}

	server := &http.Server{
		Handler:           r.handler(client, target, cluster),
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		ErrorLog:          slog.NewLogLogger(r.logger.Handler(), slog.LevelDebug),
	}
	r.mu.Lock()
	_, ok := r.endpoints[id]
	if ok {
		r.endpoints[id] = server
	}

	r.mu.Unlock()
	if !ok {
		_ = listener.Close()
		return "", errors.New("the cluster was removed while it started")
	}

	go func() { _ = server.ServeTLS(listener, "", "") }()
	return "https://" + listener.Addr().String(), nil
}

// certificate issues a certificate for 127.0.0.1 and localhost from the
// runtime's certificate authority.
func (r *Runtime) certificate() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "kube-apiserver"}, NotBefore: now.Add(-time.Hour),
		NotAfter: now.AddDate(1, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{"localhost"},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, r.ca, &key.PublicKey, r.caKey)
	if err != nil {
		return tls.Certificate{}, err
	}

	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// handler forwards the requests whose EKS token is from the cluster's account
// to its API server as the administrator, so every caller from that account
// acts as the cluster's administrator.
func (r *Runtime) handler(client *http.Client, target *url.URL, cluster eks.Cluster) http.Handler {
	proxy := &httputil.ReverseProxy{
		Transport: client.Transport,
		// Watches and logs stream.
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = ""
			// The client certificate authenticates instead of the token.
			pr.Out.Header.Del("Authorization")
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			status(w, http.StatusBadGateway, "InternalError", "The cluster's API server is unavailable")
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		account, err := r.authenticate(req, cluster.Name)
		if err != nil || account != cluster.Account {
			status(w, http.StatusUnauthorized, "Unauthorized", "Unauthorized")
			return
		}

		proxy.ServeHTTP(w, req)
	})
}

// status writes a Kubernetes Status error.
func status(w http.ResponseWriter, code int, reason, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"kind": "Status", "apiVersion": "v1", "metadata": map[string]any{}, "status": "Failure",
		"message": message, "reason": reason, "code": code,
	})
}

// ready waits until a k3s container has written its kubeconfig and its API
// server is ready, and returns the administrator's client and the server URL.
func ready(ctx context.Context, id string) (*http.Client, *url.URL, error) {
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	ports, err := dockercli.Run(ctx, nil, "port", id, "6443/tcp")
	if err != nil {
		return nil, nil, err
	}

	// "docker port" may list an IPv4 and an IPv6 binding; use the first.
	first, _, _ := strings.Cut(ports, "\n")
	target, err := url.Parse("https://127.0.0.1:" + first[strings.LastIndex(first, ":")+1:])
	if err != nil {
		return nil, nil, err
	}

	var client *http.Client
	for {
		if client == nil {
			var config string
			if config, err = dockercli.Run(ctx, nil, "exec", id, "cat", kubeconfig); err == nil {
				client, err = adminClient(config)
			}
		}

		if client != nil {
			if err = probe(ctx, client, target); err == nil {
				return client, target, nil
			}
		}

		select {
		case <-ctx.Done():
			return nil, nil, fmt.Errorf("k3s did not become ready: %w", err)
		case <-time.After(time.Second):
		}
	}
}

// probe checks that an API server answers /readyz.
func probe(ctx context.Context, client *http.Client, target *url.URL) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String()+"/readyz", http.NoBody)
	if err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("readyz answered %d", resp.StatusCode)
	}

	return nil
}

// adminClient returns an HTTP client with the certificate authority, client
// certificate, and key of a kubeconfig k3s wrote.
func adminClient(config string) (*http.Client, error) {
	var kc struct {
		Clusters []struct {
			Cluster struct {
				CA string `yaml:"certificate-authority-data"`
			} `yaml:"cluster"`
		} `yaml:"clusters"`
		Users []struct {
			User struct {
				Cert string `yaml:"client-certificate-data"`
				Key  string `yaml:"client-key-data"`
			} `yaml:"user"`
		} `yaml:"users"`
	}
	if err := yaml.Unmarshal([]byte(config), &kc); err != nil {
		return nil, fmt.Errorf("read kubeconfig: %w", err)
	}

	if len(kc.Clusters) == 0 || len(kc.Users) == 0 {
		return nil, errors.New("the kubeconfig names no cluster or user")
	}

	var ca, cert, key []byte
	for _, field := range []struct {
		to   *[]byte
		from string
	}{
		{&ca, kc.Clusters[0].Cluster.CA},
		{&cert, kc.Users[0].User.Cert},
		{&key, kc.Users[0].User.Key},
	} {
		var err error
		if *field.to, err = base64.StdEncoding.DecodeString(field.from); err != nil {
			return nil, fmt.Errorf("read kubeconfig: %w", err)
		}
	}

	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return nil, err
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, errors.New("the kubeconfig's certificate authority is not PEM")
	}

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			Certificates: []tls.Certificate{pair}, RootCAs: roots, MinVersion: tls.VersionTLS12,
		},
		ForceAttemptHTTP2: true,
	}

	return &http.Client{Transport: transport}, nil
}

// Running reports whether a container runs and this runtime serves its
// endpoint, which a restarted TinyStack does not.
func (r *Runtime) Running(ctx context.Context, id string) (bool, error) {
	r.mu.Lock()
	server := r.endpoints[id]
	r.mu.Unlock()
	if server == nil {
		return false, nil
	}

	out, err := dockercli.Run(ctx, nil, "inspect", "--format", "{{.State.Running}}", id)
	if dockercli.Gone(err) {
		return false, nil
	}

	return out == "true", err
}

// Remove closes a cluster's endpoint and deletes its container and volumes. It
// outlives the caller's cancellation, since a container must go even when its
// cluster was abandoned.
func (r *Runtime) Remove(ctx context.Context, id string) {
	r.mu.Lock()
	server := r.endpoints[id]
	delete(r.endpoints, id)
	r.mu.Unlock()
	if server != nil {
		_ = server.Close()
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), removeTimeout)
	defer cancel()
	_, _ = dockercli.Run(ctx, nil, "rm", "-f", "-v", id)
}

// Close removes every cluster the runtime started, refuses new ones, and waits
// for the removals.
func (r *Runtime) Close() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.Reset()
}

// Reset removes every cluster the runtime started and waits for the removals.
// Unlike Close, the runtime keeps serving.
func (r *Runtime) Reset() {
	r.mu.Lock()
	ids := slices.Collect(maps.Keys(r.endpoints))
	r.mu.Unlock()
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Go(func() { r.Remove(context.Background(), id) })
	}

	wg.Wait()
}

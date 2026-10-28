// Package docker runs each MSK cluster as one Kafka-compatible Redpanda broker.
package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/localtls"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	"github.com/amjadjibon/tinystack/internal/services/kafka"
)

// Image pins the Kafka-compatible engine independently of the recorded KafkaVersion.
const (
	Image      = "redpandadata/redpanda:v24.3.18"
	label      = "tinystack.kafka"
	ownerLabel = "tinystack.kafka.store"
)

type Runtime struct {
	specs  map[string]*kafka.Spec
	dir    string
	mu     sync.Mutex
	closed bool
}

func New(dataDir string) *Runtime { return &Runtime{specs: map[string]*kafka.Spec{}, dir: dataDir} }

func runArgs(s *kafka.Spec, plain, tlsPort int64) []string {
	args := []string{
		"create",
		"--name",
		s.Name,
		"--label",
		label + "=" + s.ARN,
		"--label",
		ownerLabel + "=" + s.Owner,
		"-v",
		s.Volume + ":/var/lib/redpanda/data",
	}

	listen, advertise := []string{}, []string{}
	if s.Plain {
		args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:9092/tcp", plain))
		listen = append(listen, "plain://0.0.0.0:9092")
		advertise = append(advertise, fmt.Sprintf("plain://127.0.0.1:%d", plain))
	}

	if s.TLS {
		args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:9094/tcp", tlsPort))
		listen = append(listen, "tls://0.0.0.0:9094")
		advertise = append(advertise, fmt.Sprintf("tls://127.0.0.1:%d", tlsPort))
	}

	args = append(
		args,
		"--",
		Image,
		"redpanda",
		"start",
		"--mode",
		"dev-container",
		"--node-id",
		"1",
		"--smp",
		"1",
		"--memory",
		"512M",
		"--reserve-memory",
		"0M",
		"--kafka-addr",
		strings.Join(listen, ","),
		"--advertise-kafka-addr",
		strings.Join(advertise, ","),
	)
	if s.TLS {
		args = append(
			args,
			"--set",
			`redpanda.kafka_api_tls=[{"name":"tls","enabled":true,"cert_file":"/etc/redpanda/server.pem",`+
				`"key_file":"/etc/redpanda/server-key.pem"}]`,
		)
	}

	return args
}

func (r *Runtime) Ensure(ctx context.Context, s *kafka.Spec) (kafka.Started, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return kafka.Started{}, fmt.Errorf("runtime is closed")
	}

	r.specs[s.Name] = s
	_, err := dockercli.Run(ctx, nil, "inspect", s.Name)
	if err != nil && !dockercli.Gone(err) {
		return kafka.Started{}, err
	}

	if err != nil {
		plain, portErr := dockercli.FreeLoopbackPort(ctx)
		if portErr != nil {
			return kafka.Started{}, portErr
		}

		tlsPort, portErr := dockercli.FreeLoopbackPort(ctx)
		if portErr != nil {
			return kafka.Started{}, portErr
		}

		for tlsPort == plain {
			tlsPort, portErr = dockercli.FreeLoopbackPort(ctx)
			if portErr != nil {
				return kafka.Started{}, portErr
			}
		}

		if _, err = dockercli.Run(
			ctx,
			nil,
			"volume",
			"create",
			"--label",
			label+"="+s.ARN,
			"--label",
			ownerLabel+"="+s.Owner,
			s.Volume,
		); err != nil {
			return kafka.Started{}, err
		}

		if _, err = dockercli.Run(ctx, nil, runArgs(s, plain, tlsPort)...); err != nil {
			return kafka.Started{}, err
		}

		if s.TLS {
			if err = r.copyCertificate(ctx, s.Name); err != nil {
				_, _ = dockercli.Run(ctx, nil, "rm", "-f", s.Name)
				return kafka.Started{}, err
			}
		}
	}

	if _, err = dockercli.Run(ctx, nil, "start", s.Name); err != nil {
		return kafka.Started{}, err
	}

	out := kafka.Started{}
	if s.Plain {
		out.Port, err = dockercli.HostPort(ctx, s.Name, 9092)
		if err != nil {
			return out, err
		}
	}

	if s.TLS {
		out.TLSPort, err = dockercli.HostPort(ctx, s.Name, 9094)
	}

	return out, err
}

func (r *Runtime) copyCertificate(ctx context.Context, name string) error {
	ca, err := localtls.Load(r.dir)
	if err != nil {
		return err
	}

	cfg, err := ca.Config(nil)
	if err != nil {
		return err
	}

	cert, err := cfg.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		return err
	}

	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		return err
	}

	files := map[string][]byte{
		"server-key.pem": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}),
		"server.pem":     pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}),
	}

	data := new(bytes.Buffer)
	writer := tar.NewWriter(data)
	for _, name := range []string{"server.pem", "server-key.pem"} {
		body := files[name]
		if writeErr := writer.WriteHeader(
			&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Uid: 101, Gid: 101},
		); writeErr != nil {
			return writeErr
		}

		if _, err = writer.Write(body); err != nil {
			return err
		}
	}

	if closeErr := writer.Close(); closeErr != nil {
		return closeErr
	}

	_, err = dockercli.Run(ctx, data, "cp", "-", name+":/etc/redpanda")
	return err
}

// Probe waits for the Kafka API via the broker's rpk client, including TLS configuration from redpanda.yaml.
func (r *Runtime) Probe(ctx context.Context, s *kafka.Spec) error {
	_, err := dockercli.Run(ctx, nil, "exec", s.Name, "rpk", "cluster", "health", "--exit-when-healthy")
	return err
}

func (r *Runtime) Resources(ctx context.Context, owner string) ([]*kafka.Spec, error) {
	containers, volumes, err := dockercli.ListOwned(ctx, label, ownerLabel, owner)
	if err != nil {
		return nil, err
	}

	out := []*kafka.Spec{}
	for name, arn := range containers {
		out = append(out, &kafka.Spec{Name: name, ARN: arn, Owner: owner})
	}

	for name, arn := range volumes {
		out = append(out, &kafka.Spec{Volume: name, ARN: arn, Owner: owner})
	}

	return out, nil
}

func (r *Runtime) Remove(ctx context.Context, s *kafka.Spec, volume bool) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if s.Name != "" {
		if _, err := dockercli.Run(ctx, nil, "rm", "-f", s.Name); err != nil && !dockercli.Gone(err) {
			return err
		}
	}

	if volume && s.Volume != "" {
		if _, err := dockercli.Run(ctx, nil, "volume", "rm", s.Volume); err != nil && !dockercli.Gone(err) {
			return err
		}
	}

	r.mu.Lock()
	for name, current := range r.specs {
		if name == s.Name || current.ARN == s.ARN && current.Owner == s.Owner {
			delete(r.specs, name)
		}
	}

	r.mu.Unlock()
	return nil
}

func (r *Runtime) Close() {
	r.mu.Lock()
	r.closed = true
	specs := make([]*kafka.Spec, 0, len(r.specs))
	for _, s := range r.specs {
		specs = append(specs, s)
	}

	r.mu.Unlock()
	for _, s := range specs {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, _ = dockercli.Run(ctx, nil, "stop", "--time", "10", s.Name)
		cancel()
		_ = r.Remove(context.Background(), s, false)
	}
}

var _ kafka.Runtime = (*Runtime)(nil)

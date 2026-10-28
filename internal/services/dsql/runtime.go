package dsql

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const runtimeOwnerKey = "aws/dsql/runtime-owner"

type backend struct {
	Name     string
	Password string
	Port     int64
}

type dockerRuntime struct {
	backends map[string]*backend
	owner    string
	mu       sync.Mutex
}

func (s *Service) UseDocker(ctx context.Context) error {
	s.workers.Stop()
	var owner string
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		old, found, err := storage.Read[string](tx, runtimeOwnerKey)
		if err != nil {
			return err
		}

		if found {
			owner = *old
			return nil
		}

		owner = common.RandomID(24)
		return storage.Write(tx, runtimeOwnerKey, owner)
	})
	if err != nil {
		return err
	}

	s.runtime = &dockerRuntime{owner: owner, backends: map[string]*backend{}}
	if err := s.recoverOwned(ctx); err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, k := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(k, serviceName)
			if !ok || !strings.HasPrefix(rest, kindCluster+"/") {
				continue
			}

			r, _, err := storage.Read[record](tx, k)
			if err != nil {
				return err
			}

			scope := request.Scope{AccountID: account, Region: region}
			if common.Value(r.Status) == active {
				r.Status = new(creating)
				if err := write(tx, scope, r); err != nil {
					return err
				}
			}

			if err := storage.Write(
				tx,
				indexKey(scope, common.Value(r.Identifier)),
				work{Scope: scope, ID: common.Value(r.Identifier)},
			); err != nil {
				return err
			}
		}

		return nil
	})
}

func (rt *dockerRuntime) ensure(ctx context.Context, b *backend) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	name := b.Name
	_, err := dockercli.Run(ctx, nil, "container", "inspect", "--format", "{{.Id}}", name)
	if err != nil && !dockercli.Gone(err) {
		return err
	}

	if err != nil {
		volumeOwner, volumeErr := dockercli.Run(ctx, nil, "volume", "inspect", "--format",
			"{{index .Labels \"tinystack.dsql.owner\"}}", name)
		if volumeErr != nil && !dockercli.Gone(volumeErr) {
			return volumeErr
		}

		if volumeErr == nil && volumeOwner != rt.owner {
			return fmt.Errorf("DSQL volume ownership mismatch")
		}

		labels := []string{"--label", "tinystack.dsql=" + name, "--label", "tinystack.dsql.owner=" + rt.owner}
		if _, err = dockercli.Run(
			ctx,
			nil,
			append([]string{"volume", "create"}, append(labels, name)...)...); err != nil {
			return err
		}

		args := append([]string{"run", "-d", "--name", name}, labels...)
		args = append(
			args,
			"-p",
			"127.0.0.1::5432/tcp",
			"-v",
			name+":/var/lib/postgresql/data",
			"-e",
			"POSTGRES_USER="+bootstrapRole,
			"-e",
			"POSTGRES_PASSWORD="+b.Password,
			"-e",
			"POSTGRES_DB=postgres",
			"-e",
			"POSTGRES_INITDB_ARGS=--auth-host=password",
			"-e",
			"POSTGRES_HOST_AUTH_METHOD=password",
			"postgres:16-alpine",
		)
		if _, err = dockercli.Run(ctx, nil, args...); err != nil {
			return err
		}
	} else {
		owner, ownerErr := dockercli.Run(
			ctx,
			nil,
			"container",
			"inspect",
			"--format",
			"{{index .Config.Labels \"tinystack.dsql.owner\"}}",
			name,
		)
		if ownerErr != nil {
			return ownerErr
		}

		if owner != rt.owner {
			return fmt.Errorf("DSQL container ownership mismatch")
		}

		if _, err = dockercli.Run(ctx, nil, "start", name); err != nil {
			return err
		}
	}

	rt.backends[name] = b

	port, err := dockercli.HostPort(ctx, name, 5432)
	if err != nil {
		return err
	}

	if _, err := dockercli.Run(
		ctx,
		nil,
		"exec",
		name,
		"pg_isready",
		"-h",
		"127.0.0.1",
		"-U",
		"admin",
		"-d",
		"postgres",
	); err != nil {
		return err
	}

	b.Port = port
	return nil
}

func (rt *dockerRuntime) remove(ctx context.Context, name string, volume bool) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	v := ""
	if volume {
		v = name
	}

	if err := dockercli.RemoveContainerVolume(ctx, name, v); err != nil {
		return err
	}

	delete(rt.backends, name)
	return nil
}

func (rt *dockerRuntime) close(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rt.mu.Lock()
	names := make([]string, 0, len(rt.backends))
	for name := range rt.backends {
		names = append(names, name)
	}

	rt.mu.Unlock()
	for _, name := range names {
		_ = rt.remove(ctx, name, false)
	}

	// Include a container whose creation completed while its Docker command was canceled.
	containers, _, err := dockercli.ListOwned(ctx, "tinystack.dsql", "tinystack.dsql.owner", rt.owner)
	if err == nil {
		for id := range containers {
			_ = dockercli.RemoveContainerVolume(ctx, id, "")
		}
	}
}

func backendName(owner, arn string) string {
	return "tinystack-dsql-" + owner + "-" + strings.ReplaceAll(strings.TrimPrefix(arn, "arn:aws:dsql:"), ":", "-")
}

// prepareBackend persists credentials before Docker I/O; peers select one canonical ARN.
func (s *Service) prepareBackend(ctx context.Context, job *work) bool {
	var b *backend
	var deletingBackend bool
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		if err := storage.Write(tx, runtimeOwnerKey, s.runtime.owner); err != nil {
			return err
		}

		r, err := read(tx, job.Scope, job.ID)
		if err != nil {
			return err
		}

		status := common.Value(r.Status)
		if status == deleting {
			b = r.Backend
			deletingBackend = true
			if b == nil {
				return nil
			}

			for _, k := range tx.Keys("") {
				_, _, rest, ok := storage.Scoped(k, serviceName)
				if !ok || !strings.HasPrefix(rest, kindCluster+"/") || k == key(job.Scope, job.ID) {
					continue
				}

				peer, _, readErr := storage.Read[record](tx, k)
				if readErr != nil {
					return readErr
				}

				if peer.Backend != nil && peer.Backend.Name == b.Name {
					b = nil
					break
				}
			}

			return nil
		}

		if status != creating && status != updating && status != active {
			return nil
		}

		if !peersReady(tx, job.Scope, r, false) {
			return nil
		}

		canonical := common.Value(r.Arn)
		if r.MultiRegionProperties != nil {
			for _, arn := range r.MultiRegionProperties.Clusters {
				if arn < canonical {
					canonical = arn
				}
			}
		}

		scope, id, err := parseARN(job.Scope, canonical)
		if err != nil {
			return err
		}

		primary, err := read(tx, scope, id)
		if err != nil {
			return err
		}

		if primary.Backend == nil {
			primary.Backend = &backend{Name: backendName(s.runtime.owner, canonical), Password: common.RandomID(48)}
			primary.Backend.Name = strings.ReplaceAll(primary.Backend.Name, "/", "-")
			if err := write(tx, scope, primary); err != nil {
				return err
			}
		}

		r.Backend = primary.Backend
		b = common.Clone(r.Backend)
		return write(tx, job.Scope, r)
	})
	if err != nil {
		return false
	}

	if deletingBackend {
		s.closeProxy(resourceARN(job.Scope, job.ID))
		if b == nil {
			return true
		}

		return s.runtime.remove(ctx, b.Name, true) == nil
	}

	if b == nil {
		return true
	}

	if err := s.runtime.ensure(ctx, b); err != nil {
		return false
	}

	if err := s.secureBackend(ctx, b); err != nil {
		return false
	}

	if err := s.probeBackend(ctx, b); err != nil {
		return false
	}

	var live *record
	if err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var err error
		live, err = read(tx, job.Scope, job.ID)
		return err
	}); err != nil {
		return false
	}

	live.Backend = b
	if err := s.listen(ctx, job.Scope, live); err != nil {
		return false
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := read(tx, job.Scope, job.ID)
		if err != nil {
			return err
		}

		r.Backend = b
		r.ProxyPort = live.ProxyPort
		return write(tx, job.Scope, r)
	}) == nil
}

// Reset removes only this store's owned DSQL containers and volumes.
func (s *Service) Reset(ctx context.Context) {
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	s.closeProxies()
	if s.runtime == nil {
		return
	}

	containers, volumes, err := dockercli.ListOwned(ctx, "tinystack.dsql", "tinystack.dsql.owner", s.runtime.owner)
	if err != nil {
		return
	}

	for id := range containers {
		_ = dockercli.RemoveContainerVolume(ctx, id, "")
	}

	for name := range volumes {
		_ = dockercli.RemoveContainerVolume(ctx, "", name)
	}

	// Drop pending work so the worker does not recreate a backend before the app clears the store.
	_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, k := range tx.Keys(indexPrefix) {
			tx.Delete(k)
		}

		return nil
	})
}

func (s *Service) recoverOwned(ctx context.Context) error {
	retained := map[string]bool{}
	if err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			_, _, rest, ok := storage.Scoped(k, serviceName)
			if !ok || !strings.HasPrefix(rest, kindCluster+"/") {
				continue
			}

			r, _, err := storage.Read[record](tx, k)
			if err != nil {
				return err
			}

			if r.Backend != nil {
				retained[r.Backend.Name] = true
			}
		}

		return nil
	}); err != nil {
		return err
	}

	containers, volumes, err := dockercli.ListOwned(ctx, "tinystack.dsql", "tinystack.dsql.owner", s.runtime.owner)
	if err != nil {
		return err
	}

	for id, name := range containers {
		if !retained[name] {
			if err := dockercli.RemoveContainerVolume(ctx, id, ""); err != nil {
				return err
			}
		}
	}

	for volume, name := range volumes {
		if !retained[name] {
			if err := dockercli.RemoveContainerVolume(ctx, "", volume); err != nil {
				return err
			}
		}
	}

	return nil
}

package ec2

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	vmStart  = "start"
	vmDelete = "delete"
	vmT4g    = "t4g"
)

// VMSpec contains only driver-independent launch configuration.
type VMSpec struct {
	Name      string
	Image     config.VMImage
	Arch      string
	User      string
	PublicKey string
	UserData  string
	Region    string
	Port      int
	CPUs      int
	MemoryMiB int
}

// Runtime performs all VM I/O outside storage transactions. Implementations must
// scope Resources to the exact ownership prefix and must not touch foreign VMs.
type Runtime interface {
	Resources(context.Context, string) ([]string, error)
	Ensure(context.Context, *VMSpec) error
	Action(context.Context, string, string) error
	Ready(context.Context, string) (int, bool, error)
}

func (s *Service) UseRuntime(rt Runtime, cfg config.LimaConfig, imds, gateway http.Handler) error {
	owner, err := dockercli.RuntimeOwner(context.Background(), s.store, "aws/ec2/runtime-owner")
	if err != nil {
		return err
	}

	s.runtime, s.runtimeConfig, s.owner = rt, cfg, "tinystack-ec2-"+owner[:16]+"-"
	s.imds, s.gateway, s.listeners = imds, gateway, map[string]*http.Server{}
	s.bg = background.New()
	s.bg.Every(time.Second, s.visitVMs)
	return nil
}

// Stop leaves guests running. Listeners are rebound from persisted ports on recovery.
func (s *Service) Stop() {
	if s.bg == nil {
		return
	}

	s.bg.Stop()
	s.vmMu.Lock()
	defer s.vmMu.Unlock()
	for _, server := range s.listeners {
		_ = server.Close()
	}
}

// Reset deletes only the owning store's VMs, including interrupted launches.
func (s *Service) Reset(ctx context.Context) {
	if s.runtime == nil {
		return
	}

	s.vmMu.Lock()
	defer s.vmMu.Unlock()
	names, err := s.runtime.Resources(ctx, s.owner)
	if err != nil {
		return
	}

	for _, name := range names {
		if strings.HasPrefix(name, s.owner) {
			_ = s.runtime.Action(ctx, name, vmDelete)
		}
	}

	for _, server := range s.listeners {
		_ = server.Close()
	}

	s.listeners = map[string]*http.Server{}
	_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, key := range tx.Keys("aws/ec2/instances/") {
			tx.Delete(key)
		}

		return nil
	})
}

func (s *Service) validateVM(tx storage.ReadTx, l *launch, count int64, id string) error {
	if _, _, err := vmImage(l.img, id, s.runtimeConfig.Images); err != nil {
		return err
	}

	_, memory, err := VMSize(l.instanceTyp)
	if err != nil {
		return err
	}

	if memory > s.runtimeConfig.MaxMemoryMiB {
		return awserror.New("InsufficientInstanceCapacity", "instance exceeds ec2.lima.max_memory_mib")
	}

	active := 0
	for _, key := range tx.Keys("aws/ec2/instances/") {
		partition, found, readErr := storage.Read[request.Scope](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found {
			continue
		}

		id := strings.TrimPrefix(key, "aws/ec2/instances/")
		i, found, readErr := storage.Read[instance](tx, partition.Key("ec2", kindInstance, id))
		if readErr != nil {
			return readErr
		}

		if found && i.State != stateTerminated {
			active++
		}
	}

	if int64(active)+count > int64(s.runtimeConfig.MaxVMs) {
		return awserror.New("InsufficientInstanceCapacity", "ec2.lima.max_vms exhausted (stopped VMs reserve capacity)")
	}

	data, _ := base64.StdEncoding.DecodeString(l.userData)
	return validateVMUserData(string(data))
}

// Each listener fixes the instance identity; request headers cannot select a different VM.
// Lima's host.lima.internal gateway reaches host loopback without a host network change.
func (s *Service) vmListener(ctx context.Context, i *instance) error {
	if s.listeners[i.ID] != nil {
		return nil
	}

	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", net.JoinHostPort("127.0.0.1",
		strconv.Itoa(i.VMPort)))
	if err != nil {
		return err
	}

	i.VMPort = listener.Addr().(*net.TCPAddr).Port
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request,
	) {
		if strings.HasPrefix(r.URL.Path, "/latest/") {
			r.Header.Set("X-TinyStack-Instance-Id", i.ID)
			s.imds.ServeHTTP(w, r)
			return
		}

		s.gateway.ServeHTTP(w, r)
	})}
	s.listeners[i.ID] = server
	s.bg.Go(func(ctx context.Context) {
		stop := context.AfterFunc(ctx, func() { _ = server.Close() })
		defer stop()
		_ = server.Serve(listener)
	})
	return nil
}

func (s *Service) visitVMs(ctx context.Context) {
	s.vmMu.Lock()
	defer s.vmMu.Unlock()
	var keys []string
	known := map[string]bool{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		keys = tx.Keys("aws/ec2/instances/")
		for _, key := range keys {
			scope, found, readErr := storage.Read[request.Scope](tx, key)
			if readErr != nil {
				return readErr
			}

			if !found {
				continue
			}

			id := strings.TrimPrefix(key, "aws/ec2/instances/")
			current, found, readErr := storage.Read[instance](tx, scope.Key("ec2", kindInstance, id))
			if readErr != nil {
				return readErr
			}

			if found && current.State != stateTerminated {
				known[s.owner+id] = true
			}
		}

		return nil
	})
	if err != nil {
		return
	}

	ioCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	names, err := s.runtime.Resources(ioCtx, s.owner)
	if err == nil {
		for _, name := range names {
			if strings.HasPrefix(name, s.owner) && !known[name] {
				_ = s.runtime.Action(ioCtx, name, vmDelete)
			}
		}
	}

	cancel()
	for _, key := range keys {
		if ctx.Err() != nil {
			return
		}

		s.reconcileVM(ctx, key)
	}
}

func (s *Service) reconcileVM(ctx context.Context, key string) {
	var i *instance
	var scope *request.Scope
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var err error
		scope, _, err = storage.Read[request.Scope](tx, key)
		if err != nil || scope == nil {
			return err
		}

		i, _, err = storage.Read[instance](
			tx,
			scope.Key("ec2", kindInstance, strings.TrimPrefix(key, "aws/ec2/instances/")),
		)
		return err
	})
	if err != nil || i == nil || i.State == stateTerminated || (i.State == stateStopped && i.VMAction == "") {
		return
	}

	action, changed := i.VMAction, i.Changed
	ioCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	err = s.vmListener(ioCtx, i)
	if err == nil {
		err = s.store.Update(ctx, func(tx storage.WriteTx) error {
			current, found, readErr := storage.Read[instance](tx, scope.Key("ec2", kindInstance, i.ID))
			if readErr != nil || !found {
				return readErr
			}

			current.VMPort = i.VMPort
			return storage.Write(tx, scope.Key("ec2", kindInstance, i.ID), current)
		})
	}

	name := s.owner + i.ID
	var port int
	var ready bool
	if err == nil && action != "" {
		if action == vmStart && !i.VMCreated {
			var spec VMSpec
			spec, err = s.vmSpec(ctx, *scope, i)
			if err == nil {
				err = s.runtime.Ensure(ioCtx, &spec)
			}

			if err == nil {
				i.VMCreated = true
			}
		} else {
			err = s.runtime.Action(ioCtx, name, action)
		}
	}

	if err == nil && action != shutdownStop && action != vmDelete {
		port, ready, err = s.runtime.Ready(ioCtx, name)
	}

	if ctx.Err() != nil {
		return
	}

	if err == nil && !ready && i.State == statePending && time.Since(changed) > 10*time.Minute {
		err = fmt.Errorf("guest did not become SSH-ready within ten minutes")
	}

	_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
		current, found, readErr := storage.Read[instance](tx, scope.Key("ec2", kindInstance, i.ID))
		if readErr != nil || !found {
			return readErr
		}

		if current.VMAction != action || !current.Changed.Equal(changed) {
			return nil
		}

		previousState := current.State
		current.VMPort, current.VMCreated = i.VMPort, i.VMCreated
		if err != nil {
			current.State, current.VMError = stateTerminated, "VM runtime failed: "+err.Error()
		} else {
			switch {
			case action == vmDelete:
				current.State = stateTerminated
			case action == shutdownStop:
				current.State = stateStopped
			case ready:
				current.State = stateRunning
			default:
				current.State = statePending
			}
		}

		current.VMAction = ""
		if current.State != previousState {
			current.Changed = s.now().UTC()
		}

		if ready {
			if current.Tags == nil {
				current.Tags = map[string]string{}
			}

			current.Tags["tinystack:ssh-endpoint"] = fmt.Sprintf("127.0.0.1:%d", port)
		}

		if current.State == stateTerminated {
			if releaseErr := releaseInstanceInterfaces(tx, *scope, i.ID); releaseErr != nil {
				return releaseErr
			}

			if releaseErr := releaseInstanceAddress(tx, *scope, current); releaseErr != nil {
				return releaseErr
			}
		}

		return storage.Write(tx, scope.Key("ec2", kindInstance, i.ID), current)
	})
	if err != nil {
		_ = s.runtime.Action(ioCtx, name, vmDelete)
	}

	if err != nil || action == vmDelete {
		if server := s.listeners[i.ID]; server != nil {
			_ = server.Close()
		}

		delete(s.listeners, i.ID)
	}
}

func (s *Service) vmSpec(ctx context.Context, scope request.Scope, i *instance) (VMSpec, error) {
	img, err := findImage(scope.Region, i.ImageID)
	if err != nil {
		return VMSpec{}, err
	}

	mapped, user, err := vmImage(img, i.ImageID, s.runtimeConfig.Images)
	if err != nil {
		return VMSpec{}, err
	}

	cpus, memory, err := VMSize(i.Type)
	if err != nil {
		return VMSpec{}, err
	}

	var publicKey string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		pairs, listErr := list[keyPair](tx, scope, kindKeyPair)
		if listErr != nil {
			return listErr
		}

		for _, pair := range pairs {
			if pair.Name == i.KeyName {
				publicKey = pair.PublicKey
			}
		}

		return nil
	})
	data, _ := base64.StdEncoding.DecodeString(i.UserData)
	return VMSpec{
		Name: s.owner + i.ID, Image: mapped, Arch: i.Arch, User: user, PublicKey: publicKey,
		UserData: string(data), Region: scope.Region, Port: i.VMPort, CPUs: cpus, MemoryMiB: memory,
	}, err
}

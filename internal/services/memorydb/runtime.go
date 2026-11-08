package memorydb

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/localtls"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	"github.com/amjadjibon/tinystack/internal/services/valkeydocker"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	ownerKey     = "aws/memorydb/runtime-owner"
	runtimeIndex = "aws/memorydb/index/"
	engineACL    = "ACL"
)

// Runtime performs engine I/O outside storage transactions.
type Runtime interface {
	Resources(context.Context) ([]*valkeydocker.ClusterSpec, error)
	Ensure(context.Context, *valkeydocker.ClusterSpec) (valkeydocker.Started, error)
	Command(context.Context, *valkeydocker.ClusterSpec, ...any) (string, error)
	Remove(context.Context, *valkeydocker.ClusterSpec, bool) error
	Close()
}

// UseDocker enables persistent engine execution with the local certificate authority.
func (s *Service) UseDocker(ca *localtls.Authority, logger *slog.Logger) error {
	owner, err := dockercli.RuntimeOwner(context.Background(), s.store, ownerKey)
	if err != nil {
		return err
	}

	return s.UseRuntime(valkeydocker.NewCluster(ca, owner), logger)
}

// UseRuntime prepares recovery and starts the cluster reconciler.
func (s *Service) UseRuntime(rt Runtime, logger *slog.Logger) error {
	owner, err := dockercli.RuntimeOwner(context.Background(), s.store, ownerKey)
	if err != nil {
		return err
	}

	err = s.store.Update(context.Background(), func(tx storage.WriteTx) error {
		for _, k := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(k, serviceName)
			kind, _, _ := strings.Cut(rest, "/")
			if !ok || kind != kindCluster {
				continue
			}

			r, _, readErr := storage.Read[clusterRecord](tx, k)
			if readErr != nil {
				return readErr
			}

			if r.RuntimeStatus != statusDeleting {
				if aclErr := checkRuntimeACL(
					tx,
					request.Scope{AccountID: account, Region: region},
					r,
					true,
				); aclErr != nil {
					return aclErr
				}

				r.RuntimeStatus = statusCreating
			}

			if r.Admin == "" {
				r.Admin = common.RandomID(40)
				r.RuntimeEngine, r.RuntimeVersion = r.Engine, r.EngineVersion
			}

			r.HostPort = 0
			if writeErr := storage.Write(tx, k, r); writeErr != nil {
				return writeErr
			}

			if writeErr := storage.Write(
				tx,
				indexKey(request.Scope{AccountID: account, Region: region}, r.Name),
				true,
			); writeErr != nil {
				return writeErr
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	s.owner, s.runtime, s.logger = owner, rt, logger
	s.bg = background.New()
	s.bg.Every(time.Second, s.visit)
	return nil
}

func indexKey(scope request.Scope, name string) string {
	return runtimeIndex + scope.AccountID + "/" + scope.Region + "/" + name
}

func engineImage(engine, version string) (string, error) {
	switch engine + "/" + version {
	case "redis/6.2":
		return "redis:6.2-alpine", nil
	case "redis/7.0":
		return "redis:7.0-alpine", nil
	// Redis OSS has no 7.1 upstream release; the managed 7.1 line maps to upstream 7.2.
	case "redis/7.1":
		return "redis:7.2-alpine", nil
	case "valkey/7.2":
		return "valkey/valkey:7.2-alpine", nil
	default:
		return "", invalidValue("No Docker image for engine %s %s.", engine, version)
	}
}

func (s *Service) spec(scope request.Scope, r *clusterRecord) *valkeydocker.ClusterSpec {
	resourceARN := arn(scope, kindCluster, r.Name)
	hash := sha256.Sum256([]byte(s.owner + resourceARN))
	name := fmt.Sprintf("tinystack-memorydb-%x", hash[:16])
	image, _ := engineImage(r.RuntimeEngine, r.RuntimeVersion)
	return &valkeydocker.ClusterSpec{
		Name: name, Volume: name + "-data", ARN: resourceARN, Image: image,
		Port: r.HostPort, TLS: r.TLS, Admin: r.Admin,
	}
}

func checkRuntimeACL(tx storage.ReadTx, scope request.Scope, r *clusterRecord, enabled bool) error {
	if !enabled {
		return nil
	}

	acl, err := readACL(tx, scope, r.ACLName)
	if err != nil {
		return err
	}

	for _, name := range acl.UserNames {
		if name == defaultUserName {
			continue
		}

		user, _, err := storage.Read[userRecord](tx, key(scope, kindUser, name))
		if err != nil {
			return err
		}

		if user.AuthType == authIAM {
			return invalidValue("IAM authentication is not supported in Docker mode.")
		}

		if len(user.PasswordHashes) == 0 {
			return invalidValue("User %s needs passwords set in Docker mode.", name)
		}
	}

	return nil
}

// Stop stops background I/O, removes containers and retains volumes.
func (s *Service) Stop() {
	if s.bg != nil {
		s.bg.Stop()
	}

	if s.runtime != nil {
		s.runtime.Close()
	}
}

// Reset removes this store's containers and volumes before the store is cleared.
func (s *Service) Reset(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runtime == nil {
		return
	}

	resources, err := s.runtime.Resources(ctx)
	if err == nil {
		for _, spec := range resources {
			_ = s.runtime.Remove(ctx, spec, true)
		}
	}

	_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, k := range tx.Keys(runtimeIndex) {
			tx.Delete(k)
		}

		// Keep the identity across service and app resets.
		return nil
	})
}

func (s *Service) visit(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	if err := s.store.View(
		ctx,
		func(tx storage.ReadTx) error { keys = tx.Keys(runtimeIndex); return nil },
	); err != nil {
		return
	}

	if !s.orphansDone {
		known := map[string]bool{}
		for _, k := range keys {
			parts := strings.Split(strings.TrimPrefix(k, runtimeIndex), "/")
			if len(parts) == 3 {
				known[arn(request.Scope{AccountID: parts[0], Region: parts[1]}, kindCluster, parts[2])] = true
			}
		}

		resources, err := s.runtime.Resources(ctx)
		if err != nil {
			s.logError(err)
			return
		}

		for _, spec := range resources {
			if !known[spec.ARN] {
				if err := s.runtime.Remove(ctx, spec, true); err != nil {
					s.logError(err)
					return
				}
			}
		}

		s.orphansDone = true
	}

	for _, k := range keys {
		if ctx.Err() != nil {
			return
		}

		if err := s.advance(ctx, k); err != nil && ctx.Err() == nil {
			s.logError(err)
		}
	}
}

func (s *Service) logError(err error) {
	if s.logger != nil {
		s.logger.Error("memorydb runtime", "err", err)
	}
}

func (s *Service) advance(ctx context.Context, k string) error {
	parts := strings.Split(strings.TrimPrefix(k, runtimeIndex), "/")
	if len(parts) != 3 {
		return nil
	}

	scope := request.Scope{AccountID: parts[0], Region: parts[1]}
	recordKey := key(scope, kindCluster, parts[2])
	var r *clusterRecord
	var users []*userRecord
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var readErr error
		r, _, readErr = storage.Read[clusterRecord](tx, recordKey)
		if readErr != nil || r == nil || r.RuntimeStatus == statusDeleting {
			return readErr
		}

		acl, err := readACL(tx, scope, r.ACLName)
		if err != nil {
			return err
		}

		for _, name := range acl.UserNames {
			if name == defaultUserName {
				users = append(users, defaultUser())
				continue
			}

			user, _, err := storage.Read[userRecord](tx, key(scope, kindUser, name))
			if err != nil {
				return err
			}

			if user != nil {
				users = append(users, user)
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	if r == nil {
		return nil
	}

	spec := s.spec(scope, r)
	if r.RuntimeStatus == statusDeleting {
		if removeErr := s.runtime.Remove(ctx, spec, true); removeErr != nil {
			return removeErr
		}

		return s.store.Update(ctx, func(tx storage.WriteTx) error { tx.Delete(recordKey); tx.Delete(k); return nil })
	}

	if r.RuntimeStatus == statusAvailable && r.HostPort != 0 {
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = s.configure(probeCtx, spec, users)
		cancel()
		if err == nil {
			return nil
		}
	}

	ensureCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	started, err := s.runtime.Ensure(ensureCtx, spec)
	cancel()
	if err != nil {
		return err
	}

	spec.Port = started.Port
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	readyErr := s.configure(probeCtx, spec, users)
	if readyErr != nil {
		s.logError(readyErr)
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		current, found, err := storage.Read[clusterRecord](tx, recordKey)
		// DeleteCluster may have run while the engine started; the next visit removes it.
		if err != nil || !found || current.RuntimeStatus == statusDeleting {
			return err
		}

		current.HostPort, current.Container = started.Port, started.ID
		current.RuntimeStatus = statusCreating
		var engineErr redis.Error
		if errors.As(readyErr, &engineErr) {
			current.RuntimeStatus = "create-failed"
		}

		if readyErr == nil {
			current.RuntimeStatus = statusAvailable
		}

		return storage.Write(tx, recordKey, current)
	})
}

func (s *Service) configure(ctx context.Context, spec *valkeydocker.ClusterSpec, users []*userRecord) error {
	if _, err := s.runtime.Command(ctx, spec, "PING"); err != nil {
		return err
	}

	info, err := s.runtime.Command(ctx, spec, "CLUSTER", "INFO")
	if err != nil {
		return err
	}

	if strings.Contains(info, "cluster_slots_assigned:0\r\n") {
		args := []any{"CLUSTER", "ADDSLOTS"}
		for i := range hashSlots {
			args = append(args, i)
		}

		if _, err = s.runtime.Command(ctx, spec, args...); err != nil {
			return err
		}
	}

	// Revoke stale users before granting access to new members; retain the internal management user.
	desired := map[string]bool{"__tinystack-admin": true}
	for _, user := range users {
		desired[user.Name] = true
	}

	list, err := s.runtime.Command(ctx, spec, engineACL, "LIST")
	if err != nil {
		return err
	}

	for _, line := range strings.Split(list, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || desired[fields[1]] {
			continue
		}

		args := []any{engineACL, "DELUSER", fields[1]}
		if fields[1] == defaultUserName {
			args = []any{engineACL, "SETUSER", "default", "reset", "off"}
		}

		if _, err = s.runtime.Command(ctx, spec, args...); err != nil {
			return err
		}
	}

	for _, user := range users {
		args := valkeydocker.ACLCommand(user.Name, user.AccessString, user.PasswordHashes)
		if user.AuthType == authNoPassword {
			args = append(args, "nopass")
		}

		if _, err = s.runtime.Command(ctx, spec, args...); err != nil {
			return err
		}
	}

	info, err = s.runtime.Command(ctx, spec, "CLUSTER", "INFO")
	if err != nil {
		return err
	}

	if !strings.Contains(info, "cluster_state:ok") {
		return fmt.Errorf("cluster not ready")
	}

	return nil
}

// checkRuntimeAccess asks a running engine to validate access grammar before committing a user change.
func (s *Service) checkRuntimeAccess(ctx context.Context, scope request.Scope, access string) error {
	if s.runtime == nil {
		return nil
	}

	for _, rule := range strings.Fields(access) {
		if rule == "nopass" || rule == "resetpass" || strings.HasPrefix(rule, ">") || strings.HasPrefix(rule, "<") ||
			strings.HasPrefix(rule, "#") || strings.HasPrefix(rule, "!") {
			return invalidValue("Set passwords with AuthenticationMode, not AccessString.")
		}
	}

	var records []*clusterRecord
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var readErr error
		records, readErr = storage.Records[clusterRecord](tx, scope.Prefix(serviceName, kindCluster))
		return readErr
	})
	if err != nil {
		return err
	}

	for _, r := range records {
		if r.RuntimeStatus != statusAvailable {
			continue
		}

		spec := s.spec(scope, r)
		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		args := valkeydocker.ACLCommand("__tinystack-validation", access, nil)
		_, err = s.runtime.Command(checkCtx, spec, args...)
		_, _ = s.runtime.Command(checkCtx, spec, engineACL, "DELUSER", "__tinystack-validation")
		cancel()
		if err != nil {
			return invalidValue("Engine rejected AccessString: %s", err)
		}
	}

	return nil
}

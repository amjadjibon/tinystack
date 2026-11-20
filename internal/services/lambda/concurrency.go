package lambda

import (
	"context"
	"encoding/base64"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// A function's reserved concurrency is "reserved/<name>", holding the number,
// and each provisioned concurrency configuration "provisioned/<name>/<qualifier>".
const (
	kindReserved    = "reserved"
	kindProvisioned = "provisioned"
	// maxReserved leaves the 100 unreserved executions Lambda keeps of the
	// default account limit of 1000.
	accountConcurrency = 1000
	maxReserved        = accountConcurrency - 100
	statusReady        = "READY"
	statusInProgress   = "IN_PROGRESS"
)

// provisioned tracks allocation and initialization of reusable environments.
type provisioned struct {
	Versions     map[string]int64
	Status       string
	StatusReason string
	Token        string
	Qualifier    string
	LastModified string
	Allocated    int64
	Requested    int64
}

func reservedKey(scope request.Scope, name string) string {
	return scope.Key("lambda", kindReserved, name)
}

func provisionedKey(scope request.Scope, name, qualifier string) string {
	return scope.Key("lambda", kindProvisioned, name+"/"+qualifier)
}

// plan allocates the alias's requested capacity across the versions it routes to.
func (p *provisioned) plan(tx storage.ReadTx, scope request.Scope, name string) error {
	a, found, err := loadAlias(tx, scope, name, p.Qualifier)
	if err != nil {
		return err
	}

	if !found {
		p.Versions = map[string]int64{p.Qualifier: p.Requested}
	} else {
		if a.Version == latest {
			return invalid("Provisioned Concurrency Configs cannot be applied to unpublished function versions.")
		}

		p.Versions = map[string]int64{a.Version: p.Requested}
		for version, weight := range a.Weights {
			secondary := int64(math.Round(float64(p.Requested) * weight))
			p.Versions[version] = secondary
			p.Versions[a.Version] -= secondary
		}
	}

	for version := range p.Versions {
		fn, _, err := resolve(tx, scope, name, version)
		if err != nil {
			return err
		}

		if fn.SnapStart == snapStartPublished {
			return invalid("Provisioned Concurrency is not supported for versions with SnapStart enabled")
		}
	}

	return nil
}

func (p *provisioned) poolKey(arn, version string) string {
	return arn + "/" + version + "/" + p.Token
}

func (p *provisioned) capacity(version string) int64 {
	if p.Status == statusReady {
		return p.Versions[version]
	}

	return 0
}

func (s *Service) releaseProvisioned(scope request.Scope, name string, p *provisioned) {
	arn := qualifiedARN(scope, name, p.Qualifier)
	for version := range p.Versions {
		s.runtime.Release(p.poolKey(arn, version))
	}
}

type invocationConcurrency struct {
	function, provisioned, unreserved int
	spillover, usesUnreserved         bool
}

// acquire accounts for the function's reserved limit and the provisioned slots available to the routed version.
func (s *Service) acquire(
	ctx context.Context, scope request.Scope, fn *Function,
) (func(), invocationConcurrency, error) {
	var current invocationConcurrency
	var limit int64
	var capped bool
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var readErr error
		limit, capped, readErr = reservedConcurrency(tx, scope, fn.Name)
		return readErr
	})
	if err != nil {
		return nil, current, err
	}

	key := functionARN(scope, fn.Name)
	s.slots.Lock()
	defer s.slots.Unlock()
	if capped && int64(s.running[key]) >= limit {
		return nil, current, awserror.New("TooManyRequestsException", "Rate Exceeded.")
	}

	arn, pool := fn.ARN, fn.PoolKey
	usesProvisioned := int64(s.poolRunning[pool]) < fn.provisioned
	if usesProvisioned {
		s.poolRunning[pool]++
		s.provisionedRunning[arn]++
	} else {
		fn.PoolKey = fn.onDemandPool
		current.spillover = fn.allocated > 0
		current.usesUnreserved = !capped
		if current.usesUnreserved {
			s.unreserved[scope]++
		}
	}

	s.running[key]++
	current.function = s.running[key]
	current.provisioned = s.provisionedRunning[arn]
	current.unreserved = s.unreserved[scope]
	return func() {
		s.slots.Lock()
		defer s.slots.Unlock()
		releaseSlot(s.running, key)
		if usesProvisioned {
			releaseSlot(s.poolRunning, pool)
			releaseSlot(s.provisionedRunning, arn)
		}

		if current.usesUnreserved {
			releaseSlot(s.unreserved, scope)
		}
	}, current, nil
}

func releaseSlot[K comparable](slots map[K]int, key K) {
	slots[key]--
	if slots[key] == 0 {
		delete(slots, key)
	}
}

// latestFunction resolves a request's unqualified function and checks that it
// exists.
func latestFunction(tx storage.ReadTx, m *request.Metadata, raw *string) (string, error) {
	name, q, err := resolveName(m, common.Value(raw))
	if err != nil {
		return "", err
	}

	if guardErr := unqualified(q); guardErr != nil {
		return "", guardErr
	}

	_, _, err = resolve(tx, m.Scope, name, "")
	return name, err
}

// PutFunctionConcurrency reserves concurrent executions for a function, which
// also caps them: invocations beyond it fail with TooManyRequestsException.
func (s *Service) PutFunctionConcurrency(
	ctx context.Context,
	in *api.PutFunctionConcurrencyRequest,
) (*api.Concurrency, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	n := common.Value(in.ReservedConcurrentExecutions)
	switch {
	case in.ReservedConcurrentExecutions == nil || n < 0:
		return nil, invalid("ReservedConcurrentExecutions must be 0 or more")
	case n > maxReserved:
		return nil, invalid("Specified ReservedConcurrentExecutions for function decreases account's " +
			"UnreservedConcurrentExecution below its minimum value of [100].")
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		name, resolveErr := latestFunction(tx, &m, in.FunctionName)
		if resolveErr != nil {
			return resolveErr
		}

		tx.Put(reservedKey(m.Scope, name), []byte(strconv.FormatInt(n, 10)))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.Concurrency{ReservedConcurrentExecutions: new(n)}, nil
}

// reservedConcurrency reads a function's reserved concurrency, reporting
// whether it has one.
func reservedConcurrency(tx storage.ReadTx, scope request.Scope, name string) (n int64, ok bool, err error) {
	data, ok := tx.Get(reservedKey(scope, name))
	if !ok {
		return 0, false, nil
	}

	n, err = strconv.ParseInt(string(data), 10, 64)
	return n, err == nil, err
}

func (s *Service) GetFunctionConcurrency(
	ctx context.Context,
	in *api.GetFunctionConcurrencyRequest,
) (*api.GetFunctionConcurrencyResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.GetFunctionConcurrencyResponse{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		name, resolveErr := latestFunction(tx, &m, in.FunctionName)
		if resolveErr != nil {
			return resolveErr
		}

		n, ok, readErr := reservedConcurrency(tx, m.Scope, name)
		if ok {
			out.ReservedConcurrentExecutions = new(n)
		}

		return readErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeleteFunctionConcurrency(
	ctx context.Context,
	in *api.DeleteFunctionConcurrencyRequest,
) (*api.DeleteFunctionConcurrencyOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		name, resolveErr := latestFunction(tx, &m, in.FunctionName)
		if resolveErr != nil {
			return resolveErr
		}

		tx.Delete(reservedKey(m.Scope, name))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteFunctionConcurrencyOutput{}, nil
}

// provisionedTarget resolves the function and the version or alias a
// provisioned concurrency request names, which must exist.
func provisionedTarget(tx storage.ReadTx, m *request.Metadata, raw, qualifier *string) (name, q string, err error) {
	if name, q, err = resolveName(m, common.Value(raw)); err != nil {
		return "", "", err
	}

	if q, err = withQualifier(q, qualifier); err != nil {
		return "", "", err
	}

	if q == "" || q == latest {
		return "", "", invalid("Provisioned Concurrency Configs cannot be applied to unpublished function versions.")
	}

	_, _, err = resolve(tx, m.Scope, name, q)
	return name, q, err
}

func (p *provisioned) item(scope request.Scope, name string) api.ProvisionedConcurrencyConfigListItem {
	return api.ProvisionedConcurrencyConfigListItem{
		FunctionArn:                              new(qualifiedARN(scope, name, p.Qualifier)),
		RequestedProvisionedConcurrentExecutions: new(p.Requested),
		AllocatedProvisionedConcurrentExecutions: new(p.Allocated),
		AvailableProvisionedConcurrentExecutions: new(p.Allocated),
		Status:                                   new(p.Status),
		StatusReason:                             new(p.StatusReason),
		LastModified:                             new(p.LastModified),
	}
}

// PutProvisionedConcurrencyConfig initializes environments in the background.
func (s *Service) PutProvisionedConcurrencyConfig(
	ctx context.Context,
	in *api.PutProvisionedConcurrencyConfigRequest,
) (*api.PutProvisionedConcurrencyConfigResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	n := common.Value(in.ProvisionedConcurrentExecutions)
	if n < 1 || n > maxReserved {
		return nil, invalid("ProvisionedConcurrentExecutions must be from 1 to %d", maxReserved)
	}

	s.warmMu.Lock()
	defer s.warmMu.Unlock()
	p := provisioned{
		Requested: n, LastModified: s.now().UTC().Format(timeFormat),
		Status: statusInProgress, Token: common.RandomID(32),
	}
	var name string
	var previous provisioned
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		resolved, q, resolveErr := provisionedTarget(tx, &m, in.FunctionName, in.Qualifier)
		if resolveErr != nil {
			return resolveErr
		}

		name, p.Qualifier = resolved, q
		if planErr := p.plan(tx, m.Scope, name); planErr != nil {
			return planErr
		}

		key := provisionedKey(m.Scope, name, q)
		if _, readErr := readJSON(tx, key, &previous); readErr != nil {
			return readErr
		}

		return writeJSON(tx, key, &p)
	})
	if err != nil {
		return nil, err
	}

	s.releaseProvisioned(m.Scope, name, &previous)
	s.queueWarm(ctx, m.Scope, name, &p)
	return &api.PutProvisionedConcurrencyConfigResponse{
		RequestedProvisionedConcurrentExecutions: new(n),
		AllocatedProvisionedConcurrentExecutions: new(int64(0)),
		AvailableProvisionedConcurrentExecutions: new(int64(0)),
		Status:                                   new(p.Status),
		LastModified:                             new(p.LastModified),
	}, nil
}

func (s *Service) GetProvisionedConcurrencyConfig(
	ctx context.Context,
	in *api.GetProvisionedConcurrencyConfigRequest,
) (*api.GetProvisionedConcurrencyConfigResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var p provisioned
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		name, q, resolveErr := provisionedTarget(tx, &m, in.FunctionName, in.Qualifier)
		if resolveErr != nil {
			return resolveErr
		}

		found, readErr := readJSON(tx, provisionedKey(m.Scope, name, q), &p)
		if readErr == nil && !found {
			return awserror.New("ProvisionedConcurrencyConfigNotFoundException",
				"No Provisioned Concurrency Config found for this function")
		}

		return readErr
	})
	if err != nil {
		return nil, err
	}

	return &api.GetProvisionedConcurrencyConfigResponse{
		RequestedProvisionedConcurrentExecutions: new(p.Requested),
		AllocatedProvisionedConcurrentExecutions: new(p.Allocated),
		AvailableProvisionedConcurrentExecutions: new(p.Allocated),
		StatusReason:                             new(p.StatusReason),
		Status:                                   new(p.Status), LastModified: new(p.LastModified),
	}, nil
}

func (s *Service) DeleteProvisionedConcurrencyConfig(
	ctx context.Context,
	in *api.DeleteProvisionedConcurrencyConfigRequest,
) (*api.DeleteProvisionedConcurrencyConfigOutput, error) {
	s.warmMu.Lock()
	defer s.warmMu.Unlock()
	var p provisioned
	var name string
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		resolved, q, resolveErr := provisionedTarget(tx, &m, in.FunctionName, in.Qualifier)
		if resolveErr != nil {
			return resolveErr
		}

		name = resolved
		key := provisionedKey(m.Scope, name, q)
		if _, readErr := readJSON(tx, key, &p); readErr != nil {
			return readErr
		}

		tx.Delete(key)
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.releaseProvisioned(m.Scope, name, &p)
	return &api.DeleteProvisionedConcurrencyConfigOutput{}, nil
}

// ListProvisionedConcurrencyConfigs lists a function's configurations by
// qualifier, 50 per page.
func (s *Service) ListProvisionedConcurrencyConfigs(
	ctx context.Context,
	in *api.ListProvisionedConcurrencyConfigsRequest,
) (*api.ListProvisionedConcurrencyConfigsResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	limit, err := pageSize(in.MaxItems, listLimit)
	if err != nil {
		return nil, err
	}

	start := ""
	if in.Marker != nil {
		b, decodeErr := base64.RawURLEncoding.DecodeString(*in.Marker)
		if decodeErr != nil {
			return nil, invalid("Invalid Marker")
		}

		start = string(b)
	}

	out := &api.ListProvisionedConcurrencyConfigsResponse{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		name, resolveErr := latestFunction(tx, &m, in.FunctionName)
		if resolveErr != nil {
			return resolveErr
		}

		for _, k := range tx.Keys(provisionedKey(m.Scope, name, "")) {
			var p provisioned
			if _, readErr := readJSON(tx, k, &p); readErr != nil {
				return readErr
			}

			if p.Qualifier <= start {
				continue
			}

			if int64(len(out.ProvisionedConcurrencyConfigs)) == limit {
				out.NextMarker = nextMarker(start)
				return nil
			}

			out.ProvisionedConcurrencyConfigs = append(out.ProvisionedConcurrencyConfigs, p.item(m.Scope, name))
			start = p.Qualifier
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// queueWarm is called with warmMu held, after committing the configuration. Warm jobs run one at a time
// without warmMu, so API calls never wait on container or process startup; a job whose configuration was
// replaced, deleted or reset meanwhile releases what it started instead of recording it.
func (s *Service) queueWarm(ctx context.Context, scope request.Scope, name string, p *provisioned) {
	metadata, _ := request.FromContext(ctx)
	epoch := s.warmEpoch
	s.bg.Go(func(bgctx context.Context) {
		bgctx = request.WithMetadata(bgctx, metadata)
		s.warmJobs.Lock()
		defer s.warmJobs.Unlock()
		key := provisionedKey(scope, name, p.Qualifier)
		var current provisioned
		readErr := s.store.View(bgctx, func(tx storage.ReadTx) error {
			_, err := readJSON(tx, key, &current)
			return err
		})
		if readErr != nil || current.Token != p.Token {
			return
		}

		startErr := s.warmVersions(bgctx, scope, name, p)

		s.warmMu.Lock()
		defer s.warmMu.Unlock()
		stale := epoch != s.warmEpoch
		if !stale {
			err := s.store.Update(bgctx, func(tx storage.WriteTx) error {
				if found, err := readJSON(tx, key, &current); err != nil || !found || current.Token != p.Token {
					stale = true
					return err
				}

				current.Status, current.Allocated = statusReady, p.Requested
				if startErr != nil {
					current.Status, current.StatusReason, current.Allocated = "FAILED", startErr.Error(), 0
				}

				return writeJSON(tx, key, &current)
			})
			stale = stale || err != nil
		}

		if stale || startErr != nil {
			s.releaseProvisioned(scope, name, p)
		}
	})
}

func (s *Service) warmVersions(ctx context.Context, scope request.Scope, name string, p *provisioned) error {
	runtime, ok := s.runtime.(WarmRuntime)
	if !ok {
		return fmt.Errorf("configured runtime does not support warm reuse")
	}

	for _, version := range slices.Sorted(maps.Keys(p.Versions)) {
		count := p.Versions[version]
		if count == 0 {
			continue
		}

		fn, err := s.prepareTarget(ctx, scope, name, p.Qualifier, version)
		if err != nil {
			return err
		}

		fn.PoolKey = p.poolKey(qualifiedARN(scope, name, p.Qualifier), version)
		if err := runtime.Warm(ctx, fn, int(count)); err != nil {
			return err
		}
	}

	return nil
}

// Runtime environments cannot survive a restart. Reinitialize saved configurations before advertising them as ready.
func (s *Service) resumeProvisioned(ctx context.Context) error {
	s.warmMu.Lock()
	defer s.warmMu.Unlock()
	type warmJob struct {
		scope  request.Scope
		name   string
		config provisioned
	}

	var jobs []warmJob
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, "lambda")
			if !ok {
				continue
			}

			target, ok := strings.CutPrefix(rest, kindProvisioned+"/")
			if !ok {
				continue
			}

			name, _, _ := strings.Cut(target, "/")
			var p provisioned
			if _, err := readJSON(tx, key, &p); err != nil {
				return err
			}

			p.Status, p.StatusReason, p.Allocated = statusInProgress, "", 0
			p.Token = common.RandomID(32)
			if err := writeJSON(tx, key, &p); err != nil {
				return err
			}

			jobs = append(
				jobs,
				warmJob{scope: request.Scope{AccountID: account, Region: region}, name: name, config: p},
			)
		}

		return nil
	})
	if err != nil {
		return err
	}

	for i := range jobs {
		job := &jobs[i]
		s.queueWarm(ctx, job.scope, job.name, &job.config)
	}

	return nil
}

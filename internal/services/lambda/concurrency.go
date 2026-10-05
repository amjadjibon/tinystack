package lambda

import (
	"context"
	"encoding/base64"
	"strconv"

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
	maxReserved = 900
	statusReady = "READY"
)

// provisioned is a provisioned concurrency configuration, which TinyStack
// allocates at once: no environment is started before an invocation.
type provisioned struct {
	Qualifier    string
	LastModified string
	Requested    int64
}

func reservedKey(scope request.Scope, name string) string {
	return scope.Key("lambda", kindReserved, name)
}

func provisionedKey(scope request.Scope, name, qualifier string) string {
	return scope.Key("lambda", kindProvisioned, name+"/"+qualifier)
}

// acquire takes one of the function's concurrent executions, failing when its
// reserved concurrency is in use, and returns the function that releases it.
func (s *Service) acquire(ctx context.Context, scope request.Scope, name string) (func(), error) {
	var limit int64
	var capped bool
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var readErr error
		limit, capped, readErr = reservedConcurrency(tx, scope, name)
		return readErr
	})
	if err != nil {
		return nil, err
	}

	key := functionARN(scope, name)
	s.slots.Lock()
	defer s.slots.Unlock()
	if capped && int64(s.running[key]) >= limit {
		return nil, awserror.New("TooManyRequestsException", "Rate Exceeded.")
	}

	s.running[key]++
	return func() {
		s.slots.Lock()
		defer s.slots.Unlock()
		s.running[key]--
		if s.running[key] == 0 {
			delete(s.running, key)
		}
	}, nil
}

// latestFunction resolves a request's unqualified function and checks that it
// exists.
func latestFunction(tx storage.ReadTx, m *request.Metadata, raw *string) (string, error) {
	name, q, err := resolveName(*m, common.Value(raw))
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
	if name, q, err = resolveName(*m, common.Value(raw)); err != nil {
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
		AllocatedProvisionedConcurrentExecutions: new(p.Requested),
		AvailableProvisionedConcurrentExecutions: new(p.Requested),
		Status:                                   new(statusReady),
		LastModified:                             new(p.LastModified),
	}
}

// PutProvisionedConcurrencyConfig records provisioned concurrency for a
// version or alias, which is READY at once.
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

	p := provisioned{Requested: n, LastModified: s.now().UTC().Format(timeFormat)}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		name, q, resolveErr := provisionedTarget(tx, &m, in.FunctionName, in.Qualifier)
		if resolveErr != nil {
			return resolveErr
		}

		p.Qualifier = q
		return writeJSON(tx, provisionedKey(m.Scope, name, q), &p)
	})
	if err != nil {
		return nil, err
	}

	return &api.PutProvisionedConcurrencyConfigResponse{
		RequestedProvisionedConcurrentExecutions: new(n), AllocatedProvisionedConcurrentExecutions: new(n),
		AvailableProvisionedConcurrentExecutions: new(n), Status: new(statusReady), LastModified: new(p.LastModified),
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
		AllocatedProvisionedConcurrentExecutions: new(p.Requested),
		AvailableProvisionedConcurrentExecutions: new(p.Requested),
		Status:                                   new(statusReady), LastModified: new(p.LastModified),
	}, nil
}

func (s *Service) DeleteProvisionedConcurrencyConfig(
	ctx context.Context,
	in *api.DeleteProvisionedConcurrencyConfigRequest,
) (*api.DeleteProvisionedConcurrencyConfigOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		name, q, resolveErr := provisionedTarget(tx, &m, in.FunctionName, in.Qualifier)
		if resolveErr != nil {
			return resolveErr
		}

		tx.Delete(provisionedKey(m.Scope, name, q))
		return nil
	})
	if err != nil {
		return nil, err
	}

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

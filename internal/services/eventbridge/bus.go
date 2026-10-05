package eventbridge

import (
	"context"
	"regexp"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/eventbridge"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// maxBuses is how many custom event buses an account may have in a region.
const maxBuses = 100

// bus is the record of a custom event bus. The default bus has none. Times
// are Unix milliseconds.
type bus struct {
	Tags        map[string]string `json:",omitempty"`
	Name        string
	Description string `json:",omitempty"`
	Created     int64
	Modified    int64
}

// busPattern is a custom bus name; "/" belongs to partner event buses.
var busPattern = regexp.MustCompile(`^[.\-_A-Za-z0-9]{1,256}$`)

func busPrefix(scope request.Scope) string {
	return scope.AccountID + "/" + scope.Region + "/eventbridge/bus/"
}

func busARN(scope request.Scope, name string) string {
	return "arn:aws:events:" + scope.Region + ":" + scope.AccountID + ":event-bus/" + name
}

func busNotFound(name string) error {
	return awserror.New("ResourceNotFoundException", "Event bus "+name+" does not exist.")
}

// busName resolves a bus given by name or ARN in the caller's scope, the
// default bus when it is absent.
func busName(scope request.Scope, ref *string) (string, error) {
	name := value(ref)
	if strings.HasPrefix(name, "arn:") {
		rest, ok := strings.CutPrefix(name, busARN(scope, ""))
		if !ok {
			return "", busNotFound(name)
		}

		name = rest
	}

	if name == "" {
		return defaultBus, nil
	}

	if !busPattern.MatchString(name) {
		return "", invalid("Event bus name must be 1 to 256 characters from [.-_A-Za-z0-9].")
	}

	return name, nil
}

func loadBus(tx storage.ReadTx, scope request.Scope, name string) (*bus, bool, error) {
	return storage.Read[bus](tx, busPrefix(scope)+name)
}

func saveBus(tx storage.WriteTx, scope request.Scope, b *bus) error {
	return storage.Write(tx, busPrefix(scope)+b.Name, b)
}

// checkBus reports a custom bus that does not exist.
func checkBus(tx storage.ReadTx, scope request.Scope, name string) error {
	if name == defaultBus {
		return nil
	}

	if _, ok := tx.Get(busPrefix(scope) + name); !ok {
		return busNotFound(name)
	}

	return nil
}

func checkBusDescription(description *string) error {
	if len(value(description)) > 512 {
		return invalid("Description must be at most 512 characters.")
	}

	return nil
}

// CreateEventBus creates a custom event bus. Partner event buses, which name
// an event source, are not supported.
func (s *Service) CreateEventBus(
	ctx context.Context,
	in *api.CreateEventBusRequest,
) (*api.CreateEventBusResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	name := value(in.Name)
	switch {
	case !busPattern.MatchString(name):
		return nil, invalid("Event bus name must be 1 to 256 characters from [.-_A-Za-z0-9].")
	case name == defaultBus:
		return nil, awserror.New("ResourceAlreadyExistsException", "Event bus default already exists.")
	case strings.HasPrefix(name, "aws."):
		return nil, invalid("Event bus names starting with aws. are reserved for partner event sources.")
	}

	if guardErr := checkBusDescription(in.Description); guardErr != nil {
		return nil, guardErr
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	now := s.now().UnixMilli()
	b := &bus{Name: name, Description: value(in.Description), Tags: tags, Created: now, Modified: now}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, exists := tx.Get(busPrefix(m.Scope) + name); exists {
			return awserror.New("ResourceAlreadyExistsException", "Event bus "+name+" already exists.")
		}

		if len(tx.Keys(busPrefix(m.Scope))) >= maxBuses {
			return awserror.New("LimitExceededException", "The requested resource exceeds the maximum number allowed.")
		}

		return saveBus(tx, m.Scope, b)
	})
	if err != nil {
		return nil, err
	}

	out := &api.CreateEventBusResponse{EventBusArn: new(busARN(m.Scope, name))}
	if b.Description != "" {
		out.Description = new(b.Description)
	}

	return out, nil
}

// describe renders a bus with its policy ("" for none); nil is the default
// bus.
func (b *bus) describe(scope request.Scope, policy string) api.EventBus {
	if b == nil {
		out := api.EventBus{Name: new(defaultBus), Arn: new(busARN(scope, defaultBus))}
		if policy != "" {
			out.Policy = new(policy)
		}

		return out
	}

	out := api.EventBus{
		Name:             new(b.Name),
		Arn:              new(busARN(scope, b.Name)),
		CreationTime:     new(float64(b.Created) / 1000),
		LastModifiedTime: new(float64(b.Modified) / 1000),
	}
	if b.Description != "" {
		out.Description = new(b.Description)
	}

	if policy != "" {
		out.Policy = new(policy)
	}

	return out
}

// viewBus describes the caller's bus given by name or ARN.
func (s *Service) viewBus(ctx context.Context, ref *string) (api.EventBus, error) {
	m, err := identity(ctx)
	if err != nil {
		return api.EventBus{}, err
	}

	name, err := busName(m.Scope, ref)
	if err != nil {
		return api.EventBus{}, err
	}

	var out api.EventBus
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var b *bus
		if name != defaultBus {
			var found bool
			var loadErr error
			if b, found, loadErr = loadBus(tx, m.Scope, name); loadErr != nil || !found {
				if loadErr != nil {
					return loadErr
				}

				return busNotFound(name)
			}
		}

		out = b.describe(m.Scope, policy(tx, m.Scope, name))
		return nil
	})
	return out, err
}

func (s *Service) DescribeEventBus(
	ctx context.Context,
	in *api.DescribeEventBusRequest,
) (*api.DescribeEventBusResponse, error) {
	out, err := s.viewBus(ctx, in.Name)
	if err != nil {
		return nil, err
	}

	return &api.DescribeEventBusResponse{
		Name:             out.Name,
		Arn:              out.Arn,
		Description:      out.Description,
		Policy:           out.Policy,
		CreationTime:     out.CreationTime,
		LastModifiedTime: out.LastModifiedTime,
	}, nil
}

// UpdateEventBus changes a custom bus's description.
func (s *Service) UpdateEventBus(
	ctx context.Context,
	in *api.UpdateEventBusRequest,
) (*api.UpdateEventBusResponse, error) {
	if err := checkBusDescription(in.Description); err != nil {
		return nil, err
	}

	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	name, err := busName(m.Scope, in.Name)
	if err != nil {
		return nil, err
	}

	if name == defaultBus {
		return nil, invalid("The default event bus cannot be updated.")
	}

	var b *bus
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var found bool
		var loadErr error
		if b, found, loadErr = loadBus(tx, m.Scope, name); loadErr != nil || !found {
			if loadErr != nil {
				return loadErr
			}

			return busNotFound(name)
		}

		if in.Description != nil {
			b.Description = *in.Description
		}

		b.Modified = s.now().UnixMilli()
		return saveBus(tx, m.Scope, b)
	})
	if err != nil {
		return nil, err
	}

	out := b.describe(m.Scope, "")
	return &api.UpdateEventBusResponse{Arn: out.Arn, Name: out.Name, Description: out.Description}, nil
}

// DeleteEventBus deletes a custom bus once it has no rules. Deleting a bus
// that does not exist succeeds, and the default bus cannot be deleted.
func (s *Service) DeleteEventBus(
	ctx context.Context,
	in *api.DeleteEventBusRequest,
) (*api.DeleteEventBusOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	name, err := busName(m.Scope, in.Name)
	if err != nil {
		return nil, err
	}

	if name == defaultBus {
		return nil, invalid("Cannot delete event bus default.")
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if len(tx.Keys(rulePrefix(m.Scope, name))) > 0 {
			return invalid("Event bus %s has rules; delete them before the bus.", name)
		}

		tx.Delete(busPrefix(m.Scope) + name)
		tx.Delete(policyKey(m.Scope, name))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteEventBusOutput{}, nil
}

// ListEventBuses lists the default bus and the caller's custom buses in name
// order.
func (s *Service) ListEventBuses(
	ctx context.Context,
	in *api.ListEventBusesRequest,
) (*api.ListEventBusesResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	after, size, err := page(in.NextToken, in.Limit)
	if err != nil {
		return nil, err
	}

	prefix := value(in.NamePrefix)
	var buses []api.EventBus
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var names []string
		for _, k := range tx.Keys(busPrefix(m.Scope) + prefix) {
			names = append(names, strings.TrimPrefix(k, busPrefix(m.Scope)))
		}

		if strings.HasPrefix(defaultBus, prefix) {
			names = append(names, defaultBus)
		}

		slices.Sort(names)
		for _, name := range names {
			if name <= after {
				continue
			}

			var b *bus
			if name != defaultBus {
				var loadErr error
				if b, _, loadErr = loadBus(tx, m.Scope, name); loadErr != nil {
					return loadErr
				}
			}

			buses = append(buses, b.describe(m.Scope, policy(tx, m.Scope, name)))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	out := &api.ListEventBusesResponse{EventBuses: []api.EventBus{}}
	if len(buses) > size {
		buses = buses[:size]
		out.NextToken = nextToken(value(buses[size-1].Name))
	}

	out.EventBuses = append(out.EventBuses, buses...)
	return out, nil
}

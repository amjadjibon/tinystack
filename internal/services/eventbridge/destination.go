package eventbridge

import (
	"context"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/eventbridge"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// An API destination is an HTTP endpoint that rule targets deliver to, with the connection that authorizes the
// requests. It is "<account>/<region>/eventbridge/destination/<name>"; its state is always ACTIVE.

const (
	destinationActive = "ACTIVE"
	// defaultRateLimit is the rate AWS gives a destination that sets none.
	defaultRateLimit = 300
)

var destinationMethods = []string{"POST", "GET", "HEAD", "OPTIONS", "PUT", "PATCH", "DELETE"}

// destination is an API destination's record. Times are Unix milliseconds.
type destination struct {
	Name          string
	ID            string
	Description   string `json:",omitempty"`
	ConnectionARN string
	Endpoint      string
	Method        string
	RateLimit     int64
	Created       int64
	Modified      int64
}

func destinationPrefix(scope request.Scope) string {
	return scope.AccountID + "/" + scope.Region + "/eventbridge/destination/"
}

func destinationARN(scope request.Scope, name, id string) string {
	return resourceARNPrefix(scope, "api-destination") + name + "/" + id
}

func destinationNotFound(name string) error {
	return awserror.New("ResourceNotFoundException", "Failed to find api destination with name "+name+".")
}

func (d *destination) arn(scope request.Scope) string { return destinationARN(scope, d.Name, d.ID) }

func loadDestination(tx storage.ReadTx, scope request.Scope, name string) (*destination, error) {
	if !resourceName.MatchString(name) {
		return nil, invalid("API destination name must be 1 to 64 characters from [.-_A-Za-z0-9].")
	}

	d, ok, err := storage.Read[destination](tx, destinationPrefix(scope)+name)
	if err == nil && !ok {
		return nil, destinationNotFound(name)
	}

	return d, err
}

// destinationByARN reads the API destination an ARN names, which must be in scope and still carry the ARN's ID.
func destinationByARN(tx storage.ReadTx, scope request.Scope, arn string) (*destination, error) {
	rest, ok := strings.CutPrefix(arn, resourceARNPrefix(scope, "api-destination"))
	name, id, found := strings.Cut(rest, "/")
	if !ok || !found {
		return nil, awserror.New("ResourceNotFoundException", "Failed to find api destination with arn "+arn+".")
	}

	d, err := loadDestination(tx, scope, name)
	if err == nil && d.ID != id {
		return nil, awserror.New("ResourceNotFoundException", "Failed to find api destination with arn "+arn+".")
	}

	return d, err
}

// check validates the members of a destination and that its connection exists.
func (d *destination) check(tx storage.ReadTx, scope request.Scope) error {
	switch {
	case len(d.Description) > 512:
		return invalid("Description must be at most 512 characters.")
	case !slices.Contains(destinationMethods, d.Method):
		return invalid("HttpMethod must be one of %s.", strings.Join(destinationMethods, ", "))
	case d.RateLimit < 1:
		return invalid("InvocationRateLimitPerSecond must be at least 1.")
	}

	if err := checkEndpoint("InvocationEndpoint", d.Endpoint); err != nil {
		return err
	}

	_, err := connectionByARN(tx, scope, d.ConnectionARN)
	return err
}

func (d *destination) list(scope request.Scope) api.ApiDestination {
	return api.ApiDestination{
		ApiDestinationArn: new(d.arn(scope)), Name: new(d.Name), ApiDestinationState: new(destinationActive),
		ConnectionArn: new(d.ConnectionARN), InvocationEndpoint: new(d.Endpoint), HttpMethod: new(d.Method),
		InvocationRateLimitPerSecond: new(d.RateLimit), CreationTime: seconds(d.Created),
		LastModifiedTime: seconds(d.Modified),
	}
}

func (s *Service) CreateApiDestination(
	ctx context.Context,
	in *api.CreateApiDestinationRequest,
) (*api.CreateApiDestinationResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.Name)
	if !resourceName.MatchString(name) {
		return nil, invalid("API destination name must be 1 to 64 characters from [.-_A-Za-z0-9].")
	}

	now := s.now().UnixMilli()
	d := &destination{
		Name: name, ID: newID(), Description: common.Value(in.Description),
		ConnectionARN: common.Value(in.ConnectionArn), Endpoint: common.Value(in.InvocationEndpoint),
		Method: common.Value(in.HttpMethod), RateLimit: common.Value(common.OrElse(in.InvocationRateLimitPerSecond,
			new(int64(defaultRateLimit)))),
		Created: now, Modified: now,
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if checkErr := d.check(tx, m.Scope); checkErr != nil {
			return checkErr
		}

		if _, exists := tx.Get(destinationPrefix(m.Scope) + name); exists {
			return awserror.New("ResourceAlreadyExistsException", "ApiDestination "+name+" already exists.")
		}

		return storage.Write(tx, destinationPrefix(m.Scope)+name, d)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateApiDestinationResponse{
		ApiDestinationArn: new(d.arn(m.Scope)), ApiDestinationState: new(destinationActive),
		CreationTime: seconds(d.Created), LastModifiedTime: seconds(d.Modified),
	}, nil
}

func (s *Service) DescribeApiDestination(
	ctx context.Context,
	in *api.DescribeApiDestinationRequest,
) (*api.DescribeApiDestinationResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var d *destination
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var loadErr error
		d, loadErr = loadDestination(tx, m.Scope, common.Value(in.Name))
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	l := d.list(m.Scope)
	out := &api.DescribeApiDestinationResponse{
		ApiDestinationArn: l.ApiDestinationArn, Name: l.Name, ApiDestinationState: l.ApiDestinationState,
		ConnectionArn: l.ConnectionArn, InvocationEndpoint: l.InvocationEndpoint, HttpMethod: l.HttpMethod,
		InvocationRateLimitPerSecond: l.InvocationRateLimitPerSecond, CreationTime: l.CreationTime,
		LastModifiedTime: l.LastModifiedTime,
	}
	if d.Description != "" {
		out.Description = new(d.Description)
	}

	return out, nil
}

// ListApiDestinations lists the caller's API destinations in name order, by name prefix or connection.
func (s *Service) ListApiDestinations(
	ctx context.Context,
	in *api.ListApiDestinationsRequest,
) (*api.ListApiDestinationsResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	after, size, err := page(in.NextToken, in.Limit)
	if err != nil {
		return nil, err
	}

	out := &api.ListApiDestinationsResponse{ApiDestinations: []api.ApiDestination{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(destinationPrefix(m.Scope) + common.Value(in.NamePrefix)) {
			d, _, loadErr := storage.Read[destination](tx, k)
			if loadErr != nil {
				return loadErr
			}

			switch {
			case d.Name <= after:
			case in.ConnectionArn != nil && *in.ConnectionArn != d.ConnectionARN:
			case len(out.ApiDestinations) == size:
				out.NextToken = nextToken(*out.ApiDestinations[size-1].Name)
				return nil
			default:
				out.ApiDestinations = append(out.ApiDestinations, d.list(m.Scope))
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// UpdateApiDestination changes an API destination; members the request leaves out keep their value.
func (s *Service) UpdateApiDestination(
	ctx context.Context,
	in *api.UpdateApiDestinationRequest,
) (*api.UpdateApiDestinationResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var d *destination
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var loadErr error
		if d, loadErr = loadDestination(tx, m.Scope, common.Value(in.Name)); loadErr != nil {
			return loadErr
		}

		d.Description = common.Value(common.OrElse(in.Description, &d.Description))
		d.ConnectionARN = common.Value(common.OrElse(in.ConnectionArn, &d.ConnectionARN))
		d.Endpoint = common.Value(common.OrElse(in.InvocationEndpoint, &d.Endpoint))
		d.Method = common.Value(common.OrElse(in.HttpMethod, &d.Method))
		d.RateLimit = common.Value(common.OrElse(in.InvocationRateLimitPerSecond, &d.RateLimit))
		d.Modified = s.now().UnixMilli()
		if checkErr := d.check(tx, m.Scope); checkErr != nil {
			return checkErr
		}

		return storage.Write(tx, destinationPrefix(m.Scope)+d.Name, d)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateApiDestinationResponse{
		ApiDestinationArn: new(d.arn(m.Scope)), ApiDestinationState: new(destinationActive),
		CreationTime: seconds(d.Created), LastModifiedTime: seconds(d.Modified),
	}, nil
}

func (s *Service) DeleteApiDestination(
	ctx context.Context,
	in *api.DeleteApiDestinationRequest,
) (*api.DeleteApiDestinationResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		d, loadErr := loadDestination(tx, m.Scope, common.Value(in.Name))
		if loadErr != nil {
			return loadErr
		}

		tx.Delete(destinationPrefix(m.Scope) + d.Name)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteApiDestinationResponse{}, nil
}

package bedrockagentcorecontrol

import (
	"context"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockagentcorecontrol"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// endpointRecord is a runtime endpoint. Live is the version it serves, and Target the version it is moving to;
// zero means none.
type endpointRecord struct {
	Created     time.Time
	Updated     time.Time
	Due         time.Time
	Tags        map[string]string
	Description *string
	Name        string
	ID          string
	ClientToken string
	Fingerprint string
	Status      string
	Live        int
	Target      int
}

func readEndpoint(tx storage.ReadTx, scope request.Scope, id, name string) (*endpointRecord, error) {
	e, found, err := storage.Read[endpointRecord](tx, endpointKey(scope, id, name))
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, notFound("Endpoint %s of AgentRuntime %s does not exist", name, id)
	}

	return e, nil
}

func versionPtr(n int) *string {
	if n == 0 {
		return nil
	}

	return version(n)
}

// status is the endpoint's status; every endpoint of a deleted runtime is DELETING.
func (e *endpointRecord) status(r *runtimeRecord) string {
	if r.Deleting {
		return statusDeleting
	}

	return e.Status
}

func (s *Service) CreateAgentRuntimeEndpoint(
	ctx context.Context,
	in *api.CreateAgentRuntimeEndpointRequest,
) (*api.CreateAgentRuntimeEndpointResponse, error) {
	if err := tagging.CheckSet(in.Tags); err != nil {
		return nil, invalid("%s", err.Error())
	}

	fp := common.Clone(in)
	fp.ClientToken = nil
	sum, err := fingerprint(fp)
	if err != nil {
		return nil, err
	}

	var out *api.CreateAgentRuntimeEndpointResponse
	err = s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, e := readRuntime(tx, scope, common.Value(in.AgentRuntimeId))
		if e != nil {
			return e
		}

		if r.Deleting {
			return conflict("AgentRuntime %s is being deleted", r.ID)
		}

		existing, e := storage.Records[endpointRecord](tx, endpointPrefix(scope, r.ID))
		if e != nil {
			return e
		}

		name := common.Value(in.Name)
		for _, old := range existing {
			if in.ClientToken != nil && old.ClientToken == *in.ClientToken {
				if old.Fingerprint != sum {
					return conflict("Client token %s was already used with different parameters", *in.ClientToken)
				}

				out = createdEndpoint(scope, r, old)

				return nil
			}

			if old.Name == name {
				return conflict("Endpoint %s of AgentRuntime %s already exists", name, r.ID)
			}
		}

		target := r.Latest
		if in.AgentRuntimeVersion != nil {
			if target, e = parseVersion(*in.AgentRuntimeVersion); e != nil {
				return e
			}
		}

		if _, e = readVersion(tx, scope, r.ID, target); e != nil {
			return e
		}

		now := time.Now().UTC()
		ep := &endpointRecord{
			Created: now, Updated: now, Due: now.Add(transitionDelay), Tags: common.CloneTags(in.Tags),
			Description: in.Description, Name: name, ID: newID(name), Fingerprint: sum, Status: statusCreating,
			Target: target,
		}

		if in.ClientToken != nil {
			ep.ClientToken = *in.ClientToken
		}

		out = createdEndpoint(scope, r, ep)

		return queue(tx, scope, kindEndpoint, r.ID+"/"+name, endpointKey(scope, r.ID, name), ep)
	})

	return out, err
}

func createdEndpoint(scope request.Scope, r *runtimeRecord, e *endpointRecord) *api.CreateAgentRuntimeEndpointResponse {
	return &api.CreateAgentRuntimeEndpointResponse{
		AgentRuntimeArn: new(runtimeARN(scope, r.ID)), AgentRuntimeEndpointArn: new(endpointARN(scope, r.ID, e.Name)),
		AgentRuntimeId: new(r.ID), CreatedAt: new(e.Created), EndpointName: new(e.Name), Status: new(e.status(r)),
		TargetVersion: versionPtr(e.Target),
	}
}

func (s *Service) GetAgentRuntimeEndpoint(
	ctx context.Context,
	in *api.GetAgentRuntimeEndpointRequest,
) (*api.GetAgentRuntimeEndpointResponse, error) {
	var out *api.GetAgentRuntimeEndpointResponse
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		r, e := readRuntime(tx, scope, common.Value(in.AgentRuntimeId))
		if e != nil {
			return e
		}

		ep, e := readEndpoint(tx, scope, r.ID, common.Value(in.EndpointName))
		if e != nil {
			return e
		}

		out = &api.GetAgentRuntimeEndpointResponse{
			AgentRuntimeArn: new(
				runtimeARN(scope, r.ID),
			),
			AgentRuntimeEndpointArn: new(endpointARN(scope, r.ID, ep.Name)),
			CreatedAt:               new(ep.Created),
			Description:             ep.Description,
			Id:                      new(ep.ID),
			LastUpdatedAt:           new(ep.Updated),
			LiveVersion:             versionPtr(ep.Live),
			Name:                    new(ep.Name),
			Status:                  new(ep.status(r)),
			TargetVersion:           versionPtr(ep.Target),
		}

		return nil
	})

	return out, err
}

func (s *Service) ListAgentRuntimeEndpoints(
	ctx context.Context,
	in *api.ListAgentRuntimeEndpointsRequest,
) (*api.ListAgentRuntimeEndpointsResponse, error) {
	var out *api.ListAgentRuntimeEndpointsResponse
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		r, e := readRuntime(tx, scope, common.Value(in.AgentRuntimeId))
		if e != nil {
			return e
		}

		endpoints, e := storage.Records[endpointRecord](tx, endpointPrefix(scope, r.ID))
		if e != nil {
			return e
		}

		page, next, e := common.Page(endpoints, func(ep *endpointRecord) string { return ep.Name }, in.NextToken,
			pageLimit(in.MaxResults), errBadToken)
		if e != nil {
			return e
		}

		out = &api.ListAgentRuntimeEndpointsResponse{RuntimeEndpoints: []api.AgentRuntimeEndpoint{}, NextToken: next}
		for _, ep := range page {
			out.RuntimeEndpoints = append(out.RuntimeEndpoints, api.AgentRuntimeEndpoint{
				AgentRuntimeArn: new(
					runtimeARN(scope, r.ID),
				),
				AgentRuntimeEndpointArn: new(endpointARN(scope, r.ID, ep.Name)),
				CreatedAt:               new(ep.Created),
				Description:             ep.Description,
				Id:                      new(ep.ID),
				LastUpdatedAt:           new(ep.Updated),
				LiveVersion:             versionPtr(ep.Live),
				Name:                    new(ep.Name),
				Status:                  new(ep.status(r)),
				TargetVersion:           versionPtr(ep.Target),
			})
		}

		return nil
	})

	return out, err
}

// UpdateAgentRuntimeEndpoint moves an endpoint to another version. The DEFAULT endpoint is not updated by
// callers: it follows the runtime's latest version.
func (s *Service) UpdateAgentRuntimeEndpoint(
	ctx context.Context,
	in *api.UpdateAgentRuntimeEndpointRequest,
) (*api.UpdateAgentRuntimeEndpointResponse, error) {
	if common.Value(in.EndpointName) == DefaultEndpoint {
		return nil, invalid("The DEFAULT endpoint follows the latest version of the runtime and cannot be updated")
	}

	var out *api.UpdateAgentRuntimeEndpointResponse
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, e := readRuntime(tx, scope, common.Value(in.AgentRuntimeId))
		if e != nil {
			return e
		}

		ep, e := readEndpoint(tx, scope, r.ID, common.Value(in.EndpointName))
		if e != nil {
			return e
		}

		if status := ep.status(r); status != statusReady {
			return conflict("Endpoint %s is %s and can only be updated when it is READY", ep.Name, status)
		}

		if in.AgentRuntimeVersion != nil {
			if ep.Target, e = parseVersion(*in.AgentRuntimeVersion); e != nil {
				return e
			}

			if _, e = readVersion(tx, scope, r.ID, ep.Target); e != nil {
				return e
			}
		}

		if in.Description != nil {
			ep.Description = in.Description
		}

		now := time.Now().UTC()
		ep.Status, ep.Updated, ep.Due = statusUpdating, now, now.Add(transitionDelay)
		out = &api.UpdateAgentRuntimeEndpointResponse{
			AgentRuntimeArn: new(
				runtimeARN(scope, r.ID),
			),
			AgentRuntimeEndpointArn: new(endpointARN(scope, r.ID, ep.Name)),
			CreatedAt:               new(ep.Created),
			LastUpdatedAt:           new(now),
			LiveVersion:             versionPtr(ep.Live),
			Status:                  new(statusUpdating),
			TargetVersion:           versionPtr(ep.Target),
		}

		return queue(tx, scope, kindEndpoint, r.ID+"/"+ep.Name, endpointKey(scope, r.ID, ep.Name), ep)
	})

	return out, err
}

func (s *Service) DeleteAgentRuntimeEndpoint(
	ctx context.Context,
	in *api.DeleteAgentRuntimeEndpointRequest,
) (*api.DeleteAgentRuntimeEndpointResponse, error) {
	if common.Value(in.EndpointName) == DefaultEndpoint {
		return nil, invalid("The DEFAULT endpoint is deleted with its runtime")
	}

	var out *api.DeleteAgentRuntimeEndpointResponse
	var scope request.Scope
	err := s.update(ctx, func(tx storage.WriteTx, sc request.Scope) error {
		scope = sc
		r, e := readRuntime(tx, scope, common.Value(in.AgentRuntimeId))
		if e != nil {
			return e
		}

		ep, e := readEndpoint(tx, scope, r.ID, common.Value(in.EndpointName))
		if e != nil {
			return e
		}

		out = &api.DeleteAgentRuntimeEndpointResponse{
			AgentRuntimeId: new(r.ID), EndpointName: new(ep.Name), Status: new(statusDeleting),
		}

		if ep.Status == statusDeleting {
			return nil
		}

		ep.Status, ep.Due = statusDeleting, time.Now().UTC().Add(transitionDelay)

		return queue(tx, scope, kindEndpoint, r.ID+"/"+ep.Name, endpointKey(scope, r.ID, ep.Name), ep)
	})
	if err == nil && s.observer != nil {
		s.observer.EndpointDeleted(ctx, scope, common.Value(out.AgentRuntimeId), common.Value(out.EndpointName))
	}

	return out, err
}

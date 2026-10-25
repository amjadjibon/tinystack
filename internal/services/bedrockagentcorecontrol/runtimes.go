package bedrockagentcorecontrol

import (
	"context"
	"encoding/json"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockagentcorecontrol"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// config is the settings of one runtime version. Updates replace all of them, as AWS does.
type config struct {
	Artifact       *api.AgentRuntimeArtifact
	Network        *api.NetworkConfiguration
	Authorizer     *api.AuthorizerConfiguration
	RequestHeaders *api.RequestHeaderConfiguration
	Protocol       *api.ProtocolConfiguration
	Lifecycle      *api.LifecycleConfiguration
	Description    *string
	Environment    map[string]string
	RoleARN        string
}

type runtimeRecord struct {
	Created     time.Time
	Updated     time.Time
	Due         time.Time
	Tags        map[string]string
	ID          string
	Name        string
	ClientToken string
	Fingerprint string
	// AccessKey is the creator's, which signs the SDK calls the runtime's code makes back to TinyStack.
	AccessKey string
	Latest    int
	Deleting  bool
}

type versionRecord struct {
	Created time.Time
	Updated time.Time
	Config  config
	Status  string
	Number  int
}

func readRuntime(tx storage.ReadTx, scope request.Scope, id string) (*runtimeRecord, error) {
	r, found, err := storage.Read[runtimeRecord](tx, runtimeKey(scope, id))
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, notFound("AgentRuntime %s does not exist", id)
	}

	return r, nil
}

func readVersion(tx storage.ReadTx, scope request.Scope, id string, number int) (*versionRecord, error) {
	v, found, err := storage.Read[versionRecord](tx, versionKey(scope, id, number))
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, notFound("Version %d of AgentRuntime %s does not exist", number, id)
	}

	return v, nil
}

// status is the runtime's status: that of its latest version, or DELETING.
func (r *runtimeRecord) status(latest *versionRecord) string {
	if r.Deleting {
		return statusDeleting
	}

	return latest.Status
}

// requireRole requires arn to name an IAM role that exists in TinyStack.
func (s *Service) requireRole(ctx context.Context, arn string) error {
	_, found, err := s.roles.RoleByARN(ctx, arn)
	if err != nil {
		return err
	}

	if !found {
		return notFound("IAM role %s does not exist", arn)
	}

	return nil
}

// settings gathers the members Create and Update share.
type settings struct {
	Artifact       *api.AgentRuntimeArtifact
	RoleArn        *string
	Network        *api.NetworkConfiguration
	Description    *string
	Authorizer     *api.AuthorizerConfiguration
	RequestHeaders *api.RequestHeaderConfiguration
	Protocol       *api.ProtocolConfiguration
	Lifecycle      *api.LifecycleConfiguration
	Environment    map[string]string
}

func (c settings) config() config {
	return config{
		Artifact: c.Artifact, Network: c.Network, Authorizer: c.Authorizer, RequestHeaders: c.RequestHeaders,
		Protocol: c.Protocol, Lifecycle: common.Clone(c.Lifecycle), Description: c.Description,
		Environment: common.CloneTags(c.Environment), RoleARN: common.Value(c.RoleArn),
	}
}

// normalize checks what the model cannot express and fills in the lifecycle defaults.
func (s *Service) normalize(tx storage.ReadTx, scope request.Scope, c *config) error {
	if err := checkArtifact(c.Artifact); err != nil {
		return err
	}

	if c.Protocol != nil && common.Value(c.Protocol.ServerProtocol) == "AGUI" {
		return invalid("serverProtocol AGUI is not supported; use HTTP, MCP or A2A")
	}

	if c.Authorizer != nil && c.Authorizer.CustomJWTAuthorizer == nil {
		return invalid("authorizerConfiguration must set customJWTAuthorizer")
	}

	if c.RequestHeaders != nil && c.RequestHeaders.RequestHeaderAllowlist == nil {
		return invalid("requestHeaderConfiguration must set requestHeaderAllowlist")
	}

	if c.Lifecycle != nil && c.Lifecycle.IdleRuntimeSessionTimeout != nil && c.Lifecycle.MaxLifetime != nil &&
		*c.Lifecycle.IdleRuntimeSessionTimeout > *c.Lifecycle.MaxLifetime {
		return invalid("idleRuntimeSessionTimeout must not exceed maxLifetime")
	}

	if c.Lifecycle == nil {
		c.Lifecycle = &api.LifecycleConfiguration{}
	}

	c.Lifecycle.IdleRuntimeSessionTimeout = common.OrElse(
		c.Lifecycle.IdleRuntimeSessionTimeout,
		new(int64(defaultIdleTimeout)),
	)
	c.Lifecycle.MaxLifetime = common.OrElse(c.Lifecycle.MaxLifetime, new(int64(defaultMaxLifetime)))

	return s.checkNetwork(tx, scope, c.Network)
}

func checkArtifact(a *api.AgentRuntimeArtifact) error {
	switch {
	case a == nil || a.ContainerConfiguration == nil && a.CodeConfiguration == nil:
		return invalid("agentRuntimeArtifact must set containerConfiguration")
	case a.ContainerConfiguration != nil && a.CodeConfiguration != nil:
		return invalid("agentRuntimeArtifact must set exactly one of containerConfiguration and codeConfiguration")
	case a.CodeConfiguration != nil:
		return invalid("codeConfiguration is not supported: TinyStack has no managed Python or Node.js runtime, " +
			"so deploy the agent as a container image with containerConfiguration")
	}

	return nil
}

// checkNetwork accepts PUBLIC, and VPC when the subnets and security groups exist in TinyStack's EC2. A VPC
// runtime is stored and described, but TinyStack's containers are not placed in the VPC.
func (s *Service) checkNetwork(tx storage.ReadTx, scope request.Scope, n *api.NetworkConfiguration) error {
	if n == nil {
		return nil
	}

	switch common.Value(n.NetworkMode) {
	case "PUBLIC":
		if n.NetworkModeConfig != nil {
			return invalid("networkModeConfig is only valid when networkMode is VPC")
		}

		return nil
	case "VPC":
		if n.NetworkModeConfig == nil {
			return invalid("networkModeConfig is required when networkMode is VPC")
		}
	default:
		return invalid("networkMode must be PUBLIC or VPC")
	}

	vpc := ""
	seen := map[string]bool{}
	for _, id := range n.NetworkModeConfig.Subnets {
		if seen[id] {
			return invalid("subnets must not repeat %s", id)
		}

		seen[id] = true
		subnet, err := s.network.MountSubnet(tx, scope, id)
		if err != nil {
			return invalid("subnet %s does not exist", id)
		}

		if vpc == "" {
			vpc = common.Value(subnet.VpcId)
		}

		if common.Value(subnet.VpcId) != vpc {
			return invalid("subnets must belong to one VPC")
		}
	}

	seen = map[string]bool{}
	for _, id := range n.NetworkModeConfig.SecurityGroups {
		if seen[id] {
			return invalid("securityGroups must not repeat %s", id)
		}

		seen[id] = true
	}

	if _, err := s.network.MountGroups(tx, scope, vpc, n.NetworkModeConfig.SecurityGroups); err != nil {
		return invalid("securityGroups must exist in the VPC of the subnets")
	}

	return nil
}

func fingerprint(v any) (string, error) {
	b, err := json.Marshal(v)

	return string(b), err
}

func (s *Service) CreateAgentRuntime(
	ctx context.Context,
	in *api.CreateAgentRuntimeRequest,
) (*api.CreateAgentRuntimeResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if err = tagging.CheckSet(in.Tags); err != nil {
		return nil, invalid("%s", err.Error())
	}

	err = s.requireRole(ctx, common.Value(in.RoleArn))
	if err != nil {
		return nil, err
	}

	fp := common.Clone(in)
	fp.ClientToken = nil
	sum, err := fingerprint(fp)
	if err != nil {
		return nil, err
	}

	c := settings{
		Artifact: in.AgentRuntimeArtifact, RoleArn: in.RoleArn, Network: in.NetworkConfiguration,
		Description: in.Description, Authorizer: in.AuthorizerConfiguration,
		RequestHeaders: in.RequestHeaderConfiguration, Protocol: in.ProtocolConfiguration,
		Lifecycle: in.LifecycleConfiguration, Environment: in.EnvironmentVariables,
	}.config()
	var out *api.CreateAgentRuntimeResponse
	err = s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		existing, e := storage.Records[runtimeRecord](tx, scope.Prefix(serviceName, kindRuntime))
		if e != nil {
			return e
		}

		for _, r := range existing {
			if in.ClientToken != nil && r.ClientToken == *in.ClientToken {
				if r.Fingerprint != sum {
					return conflict("Client token %s was already used with different parameters", *in.ClientToken)
				}

				v, readErr := readVersion(tx, scope, r.ID, 1)
				if readErr != nil {
					return readErr
				}

				out = &api.CreateAgentRuntimeResponse{
					AgentRuntimeArn: new(runtimeARN(scope, r.ID)), AgentRuntimeId: new(r.ID),
					AgentRuntimeVersion: version(1), CreatedAt: new(r.Created), Status: new(r.status(v)),
				}

				return nil
			}

			if r.Name == common.Value(in.AgentRuntimeName) {
				return conflict("AgentRuntime %s already exists", r.Name)
			}
		}

		e = s.normalize(tx, scope, &c)
		if e != nil {
			return e
		}

		now := time.Now().UTC()
		r := &runtimeRecord{
			Created: now, Updated: now, Due: now.Add(transitionDelay), Tags: common.CloneTags(in.Tags),
			ID: newID(common.Value(in.AgentRuntimeName)), Name: common.Value(in.AgentRuntimeName),
			Fingerprint: sum, AccessKey: m.AccessKey, Latest: 1,
		}

		if in.ClientToken != nil {
			r.ClientToken = *in.ClientToken
		}

		v := &versionRecord{Created: now, Updated: now, Config: c, Status: statusCreating, Number: 1}
		e = storage.Write(tx, versionKey(scope, r.ID, 1), v)
		if e != nil {
			return e
		}

		def := &endpointRecord{
			Created: now, Updated: now, Name: DefaultEndpoint, ID: newID(DefaultEndpoint), Status: statusCreating,
			Target: 1,
		}

		e = storage.Write(tx, endpointKey(scope, r.ID, DefaultEndpoint), def)
		if e != nil {
			return e
		}

		out = &api.CreateAgentRuntimeResponse{
			AgentRuntimeArn: new(runtimeARN(scope, r.ID)), AgentRuntimeId: new(r.ID), AgentRuntimeVersion: version(1),
			CreatedAt: new(now), Status: new(statusCreating),
		}

		return queue(tx, scope, kindRuntime, r.ID, runtimeKey(scope, r.ID), r)
	})

	return out, err
}

// runtimeAt returns the runtime and the version number selects, or its latest.
func runtimeAt(
	tx storage.ReadTx,
	scope request.Scope,
	id string,
	number *string,
) (*runtimeRecord, *versionRecord, error) {
	r, err := readRuntime(tx, scope, id)
	if err != nil {
		return nil, nil, err
	}

	n := r.Latest
	if number != nil {
		if n, err = parseVersion(*number); err != nil {
			return nil, nil, err
		}
	}

	v, err := readVersion(tx, scope, id, n)

	return r, v, err
}

func (s *Service) GetAgentRuntime(
	ctx context.Context,
	in *api.GetAgentRuntimeRequest,
) (*api.GetAgentRuntimeResponse, error) {
	var out *api.GetAgentRuntimeResponse
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		r, v, e := runtimeAt(tx, scope, common.Value(in.AgentRuntimeId), in.AgentRuntimeVersion)
		if e != nil {
			return e
		}

		status := v.Status
		if r.Deleting {
			status = statusDeleting
		}

		c := &v.Config
		out = &api.GetAgentRuntimeResponse{
			AgentRuntimeArn: new(runtimeARN(scope, r.ID)), AgentRuntimeArtifact: c.Artifact, AgentRuntimeId: new(r.ID),
			AgentRuntimeName: new(r.Name), AgentRuntimeVersion: version(v.Number),
			AuthorizerConfiguration: c.Authorizer, CreatedAt: new(r.Created), Description: c.Description,
			EnvironmentVariables: c.Environment, LastUpdatedAt: new(v.Updated), LifecycleConfiguration: c.Lifecycle,
			NetworkConfiguration: c.Network, ProtocolConfiguration: c.Protocol,
			RequestHeaderConfiguration: c.RequestHeaders, RoleArn: new(c.RoleARN), Status: new(status),
		}

		return nil
	})

	return out, err
}

func summary(scope request.Scope, r *runtimeRecord, v *versionRecord) api.AgentRuntime {
	status := v.Status
	if r.Deleting {
		status = statusDeleting
	}

	return api.AgentRuntime{
		AgentRuntimeArn: new(
			runtimeARN(scope, r.ID),
		),
		AgentRuntimeId:      new(r.ID),
		AgentRuntimeVersion: version(v.Number),
		AgentRuntimeName:    new(r.Name),
		Description:         v.Config.Description,
		LastUpdatedAt:       new(v.Updated),
		Status:              new(status),
	}
}

func (s *Service) ListAgentRuntimes(
	ctx context.Context,
	in *api.ListAgentRuntimesRequest,
) (*api.ListAgentRuntimesResponse, error) {
	var out *api.ListAgentRuntimesResponse
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		runtimes, e := storage.Records[runtimeRecord](tx, scope.Prefix(serviceName, kindRuntime))
		if e != nil {
			return e
		}

		page, next, e := common.Page(runtimes, func(r *runtimeRecord) string { return r.ID }, in.NextToken,
			pageLimit(in.MaxResults), errBadToken)
		if e != nil {
			return e
		}

		out = &api.ListAgentRuntimesResponse{AgentRuntimes: []api.AgentRuntime{}, NextToken: next}
		for _, r := range page {
			v, e := readVersion(tx, scope, r.ID, r.Latest)
			if e != nil {
				return e
			}

			out.AgentRuntimes = append(out.AgentRuntimes, summary(scope, r, v))
		}

		return nil
	})

	return out, err
}

func (s *Service) ListAgentRuntimeVersions(
	ctx context.Context,
	in *api.ListAgentRuntimeVersionsRequest,
) (*api.ListAgentRuntimeVersionsResponse, error) {
	var out *api.ListAgentRuntimeVersionsResponse
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		r, e := readRuntime(tx, scope, common.Value(in.AgentRuntimeId))
		if e != nil {
			return e
		}

		versions, e := storage.Records[versionRecord](tx, versionPrefix(scope, r.ID))
		if e != nil {
			return e
		}

		page, next, e := common.Page(
			versions,
			func(v *versionRecord) string { return versionKey(scope, r.ID, v.Number) },
			in.NextToken,
			pageLimit(in.MaxResults),
			errBadToken,
		)
		if e != nil {
			return e
		}

		out = &api.ListAgentRuntimeVersionsResponse{AgentRuntimes: []api.AgentRuntime{}, NextToken: next}
		for _, v := range page {
			out.AgentRuntimes = append(out.AgentRuntimes, summary(scope, r, v))
		}

		return nil
	})

	return out, err
}

func (s *Service) UpdateAgentRuntime(
	ctx context.Context,
	in *api.UpdateAgentRuntimeRequest,
) (*api.UpdateAgentRuntimeResponse, error) {
	if err := s.requireRole(ctx, common.Value(in.RoleArn)); err != nil {
		return nil, err
	}

	c := settings{
		Artifact: in.AgentRuntimeArtifact, RoleArn: in.RoleArn, Network: in.NetworkConfiguration,
		Description: in.Description, Authorizer: in.AuthorizerConfiguration,
		RequestHeaders: in.RequestHeaderConfiguration, Protocol: in.ProtocolConfiguration,
		Lifecycle: in.LifecycleConfiguration, Environment: in.EnvironmentVariables,
	}.config()
	var out *api.UpdateAgentRuntimeResponse
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, e := readRuntime(tx, scope, common.Value(in.AgentRuntimeId))
		if e != nil {
			return e
		}

		latest, e := readVersion(tx, scope, r.ID, r.Latest)
		if e != nil {
			return e
		}

		if status := r.status(latest); status != statusReady {
			return conflict("AgentRuntime %s is %s and can only be updated when it is READY", r.ID, status)
		}

		e = s.normalize(tx, scope, &c)
		if e != nil {
			return e
		}

		now := time.Now().UTC()
		r.Latest++
		r.Updated, r.Due = now, now.Add(transitionDelay)
		v := &versionRecord{Created: now, Updated: now, Config: c, Status: statusUpdating, Number: r.Latest}
		e = storage.Write(tx, versionKey(scope, r.ID, v.Number), v)
		if e != nil {
			return e
		}

		def, e := readEndpoint(tx, scope, r.ID, DefaultEndpoint)
		if e != nil {
			return e
		}

		def.Status, def.Target, def.Updated = statusUpdating, v.Number, now
		e = storage.Write(tx, endpointKey(scope, r.ID, DefaultEndpoint), def)
		if e != nil {
			return e
		}

		out = &api.UpdateAgentRuntimeResponse{
			AgentRuntimeArn: new(
				runtimeARN(scope, r.ID),
			),
			AgentRuntimeId:      new(r.ID),
			AgentRuntimeVersion: version(v.Number),
			CreatedAt:           new(r.Created),
			LastUpdatedAt:       new(now),
			Status:              new(statusUpdating),
		}

		return queue(tx, scope, kindRuntime, r.ID, runtimeKey(scope, r.ID), r)
	})

	return out, err
}

// DeleteAgentRuntime deletes a runtime with its versions and endpoints, or, with a version, that one version
// when no endpoint uses it. A version is removed at once; a runtime stays DELETING until the worker removes it.
func (s *Service) DeleteAgentRuntime(
	ctx context.Context,
	in *api.DeleteAgentRuntimeRequest,
) (*api.DeleteAgentRuntimeResponse, error) {
	var out *api.DeleteAgentRuntimeResponse
	var scope request.Scope
	deleted := 0
	err := s.update(ctx, func(tx storage.WriteTx, sc request.Scope) error {
		scope = sc
		r, e := readRuntime(tx, scope, common.Value(in.AgentRuntimeId))
		if e != nil {
			return e
		}

		out = &api.DeleteAgentRuntimeResponse{AgentRuntimeId: new(r.ID), Status: new(statusDeleting)}
		if in.AgentRuntimeVersion != nil {
			deleted, e = s.deleteVersion(tx, scope, r, *in.AgentRuntimeVersion)
			out.AgentRuntimeVersion = in.AgentRuntimeVersion

			return e
		}

		if r.Deleting {
			return nil
		}

		r.Deleting, r.Due = true, time.Now().UTC().Add(transitionDelay)

		return queue(tx, scope, kindRuntime, r.ID, runtimeKey(scope, r.ID), r)
	})
	if err != nil || s.observer == nil {
		return out, err
	}

	switch {
	case deleted > 0:
		s.observer.VersionDeleted(ctx, scope, common.Value(out.AgentRuntimeId), deleted)
	case in.AgentRuntimeVersion == nil:
		s.observer.RuntimeDeleted(ctx, scope, common.Value(out.AgentRuntimeId))
	}

	return out, nil
}

func (s *Service) deleteVersion(tx storage.WriteTx, scope request.Scope, r *runtimeRecord, ref string) (int, error) {
	n, err := parseVersion(ref)
	if err != nil {
		return 0, err
	}

	if _, err = readVersion(tx, scope, r.ID, n); err != nil {
		return 0, err
	}

	endpoints, err := storage.Records[endpointRecord](tx, endpointPrefix(scope, r.ID))
	if err != nil {
		return 0, err
	}

	for _, e := range endpoints {
		if e.Live == n || e.Target == n {
			return 0, conflict("Version %d is used by endpoint %s", n, e.Name)
		}
	}

	tx.Delete(versionKey(scope, r.ID, n))

	return n, nil
}

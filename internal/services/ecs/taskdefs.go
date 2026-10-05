package ecs

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/ecs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	awsvpc           = "awsvpc"
	bridge           = "bridge"
	host             = "host"
	deleteInProgress = "DELETE_IN_PROGRESS"
	statusAll        = "ALL"
)

// taskDef is a task definition revision's record: its description and tags.
type taskDef struct {
	Def  api.TaskDefinition
	Tags []api.Tag
}

// family counts a family's revisions, which are never reused.
type family struct {
	Last int64
}

func familyKey(scope request.Scope, name string) string {
	return scope.Prefix(service, "family") + name
}

func taskDefKey(scope request.Scope, name string, revision int64) string {
	return fmt.Sprintf("%s%s/%08d", scope.Prefix(service, "taskdef"), name, revision)
}

func taskDefARN(scope request.Scope, name string, revision int64) string {
	return arn(scope, fmt.Sprintf("task-definition/%s:%d", name, revision))
}

// resolveDef returns the revision ref (family, family:revision, or an ARN)
// names: a family alone names its latest active revision.
func resolveDef(tx storage.ReadTx, scope request.Scope, ref *string) (*taskDef, error) {
	unknown := clientError("Unable to describe task definition.")
	name := common.Value(ref)
	if strings.HasPrefix(name, "arn:") {
		var ok bool
		if name, ok = strings.CutPrefix(name, arn(scope, "task-definition/")); !ok {
			return nil, unknown
		}
	}

	name, rev, hasRevision := strings.Cut(name, ":")
	if !namePattern.MatchString(name) {
		return nil, unknown
	}

	if hasRevision {
		revision, err := strconv.ParseInt(rev, 10, 64)
		if err != nil || revision < 1 {
			return nil, unknown
		}

		d, ok, err := storage.Read[taskDef](tx, taskDefKey(scope, name, revision))
		if err != nil || !ok {
			return nil, common.ErrorsOr(err, unknown)
		}

		return d, nil
	}

	defs, err := storage.Records[taskDef](tx, scope.Prefix(service, "taskdef")+name+"/")
	if err != nil {
		return nil, err
	}

	for _, d := range slices.Backward(defs) {
		if common.Value(d.Def.Status) == active {
			return d, nil
		}
	}

	return nil, unknown
}

// activeDef resolves ref to a revision that new tasks may use.
func activeDef(tx storage.ReadTx, scope request.Scope, ref *string) (*taskDef, error) {
	d, err := resolveDef(tx, scope, ref)
	if err != nil {
		return nil, err
	}

	if common.Value(d.Def.Status) != active {
		return nil, clientError("TaskDefinition is inactive")
	}

	return d, nil
}

// units converts a task size such as "1024", "1 vCPU", or "2 GB" to CPU
// units or MiB.
func units(size string) (int64, bool) {
	fields := strings.Fields(strings.ToLower(size))
	if len(fields) == 0 || len(fields) > 2 {
		return 0, false
	}

	n, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || n <= 0 {
		return 0, false
	}

	if len(fields) == 2 {
		if fields[1] != "vcpu" && fields[1] != "gb" {
			return 0, false
		}

		n *= 1024
	}

	return int64(n), true
}

// fargateMemory lists the memory sizes each Fargate CPU size allows, in MiB:
// from, to, and step. 256 CPU units also take 512 MiB.
var fargateMemory = map[int64][3]int64{
	256:   {1024, 2048, 1024},
	512:   {1024, 4096, 1024},
	1024:  {2048, 8192, 1024},
	2048:  {4096, 16384, 1024},
	4096:  {8192, 30720, 1024},
	8192:  {16384, 61440, 4096},
	16384: {32768, 122880, 8192},
}

// fargateSize reports whether a task's CPU units and MiB are a Fargate size.
func fargateSize(cpu, memory int64) bool {
	r, ok := fargateMemory[cpu]
	return ok && memory >= r[0] && memory <= r[1] && (memory-r[0])%r[2] == 0 || cpu == 256 && memory == 512
}

func (s *Service) RegisterTaskDefinition(
	ctx context.Context,
	in *api.RegisterTaskDefinitionRequest,
) (*api.RegisterTaskDefinitionResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if nameErr := checkName(in.Family, "family"); nameErr != nil {
		return nil, nameErr
	}

	def, err := newDefinition(in)
	if err != nil {
		return nil, err
	}

	if tagErr := checkTags(in.Tags); tagErr != nil {
		return nil, tagErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		f, _, readErr := storage.Read[family](tx, familyKey(scope, *in.Family))
		if readErr != nil {
			return readErr
		}

		if f == nil {
			f = &family{}
		}

		f.Last++
		def.Revision = new(f.Last)
		def.TaskDefinitionArn = new(taskDefARN(scope, *in.Family, f.Last))
		def.RegisteredAt = seconds(s.now().UTC())
		def.RegisteredBy = new("arn:aws:iam::" + scope.AccountID + ":root")
		if writeErr := storage.Write(tx, familyKey(scope, *in.Family), f); writeErr != nil {
			return writeErr
		}

		return storage.Write(tx, taskDefKey(scope, *in.Family, f.Last), taskDef{Def: *def, Tags: in.Tags})
	})
	if err != nil {
		return nil, err
	}

	return &api.RegisterTaskDefinitionResponse{TaskDefinition: def, Tags: in.Tags}, nil
}

// newDefinition checks a registration and returns its description, with the
// defaults ECS fills in.
func newDefinition(in *api.RegisterTaskDefinitionRequest) (*api.TaskDefinition, error) {
	if !oneOf(in.NetworkMode, bridge, host, awsvpc, "none") {
		return nil, invalidParameter("networkMode must be bridge, host, awsvpc, or none.")
	}

	def := &api.TaskDefinition{
		Family: in.Family, TaskRoleArn: in.TaskRoleArn, ExecutionRoleArn: in.ExecutionRoleArn,
		NetworkMode: in.NetworkMode, Volumes: in.Volumes, Status: new(active), PidMode: in.PidMode,
		IpcMode: in.IpcMode, EphemeralStorage: in.EphemeralStorage, RuntimePlatform: in.RuntimePlatform,
		RequiresCompatibilities: in.RequiresCompatibilities, Compatibilities: []string{"EC2"},
	}
	if err := checkSize(def, in); err != nil {
		return nil, err
	}

	if !oneOf(in.PidMode, host, "task") || !oneOf(in.IpcMode, host, "task", "none") {
		return nil, invalidParameter("pidMode must be host or task, and ipcMode host, task, or none.")
	}

	if e := in.EphemeralStorage; e != nil && (common.Value(e.SizeInGiB) < 21 || common.Value(e.SizeInGiB) > 200) {
		return nil, invalidParameter("ephemeralStorage sizeInGiB must be between 21 and 200.")
	}

	volumes := map[string]bool{}
	for _, v := range in.Volumes {
		if !namePattern.MatchString(common.Value(v.Name)) || volumes[*v.Name] {
			return nil, clientError("Volume names must be unique and up to 255 letters, numbers, hyphens, and " +
				"underscores.")
		}

		volumes[*v.Name] = true
	}

	containers, err := checkContainers(in, volumes)
	if err != nil {
		return nil, err
	}

	def.ContainerDefinitions = containers
	return def, nil
}

// checkSize validates the task's CPU and memory and its Fargate needs.
func checkSize(def *api.TaskDefinition, in *api.RegisterTaskDefinitionRequest) error {
	var cpu, memory int64
	if in.Cpu != nil {
		var ok bool
		if cpu, ok = units(*in.Cpu); !ok {
			return clientError("Invalid 'cpu' setting for task.")
		}

		def.Cpu = new(strconv.FormatInt(cpu, 10))
	}

	if in.Memory != nil {
		var ok bool
		if memory, ok = units(*in.Memory); !ok {
			return clientError("Invalid 'memory' setting for task.")
		}

		def.Memory = new(strconv.FormatInt(memory, 10))
	}

	for _, c := range in.RequiresCompatibilities {
		if c != "EC2" && c != fargate && c != "EXTERNAL" {
			return invalidParameter("requiresCompatibilities values are EC2, FARGATE, and EXTERNAL.")
		}
	}

	fits := common.Value(in.NetworkMode) == awsvpc && fargateSize(cpu, memory)
	if slices.Contains(in.RequiresCompatibilities, fargate) {
		switch {
		case common.Value(in.NetworkMode) != awsvpc:
			return clientError("Fargate only supports network mode ‘awsvpc’.")
		case !fits:
			return clientError("No Fargate configuration exists for given values: %s CPU, %s memory.",
				common.Value(in.Cpu), common.Value(in.Memory))
		}

		for _, v := range in.Volumes {
			if v.Host != nil && v.Host.SourcePath != nil {
				return clientError("Fargate does not support host volumes with a source path.")
			}
		}
	}

	if fits {
		def.Compatibilities = append(def.Compatibilities, fargate)
	}

	return nil
}

var logDrivers = []string{"json-file", "syslog", "journald", "gelf", "fluentd", "awslogs", "splunk", "awsfirelens"}

// checkContainers validates the container definitions and fills in ECS's
// defaults: containers are essential, and ports are TCP and, outside bridge
// mode, published on the container port.
func checkContainers(
	in *api.RegisterTaskDefinitionRequest,
	volumes map[string]bool,
) ([]api.ContainerDefinition, error) {
	if len(in.ContainerDefinitions) == 0 {
		return nil, clientError("Container list cannot be empty.")
	}

	names := map[string]bool{}
	for i := range in.ContainerDefinitions {
		c := &in.ContainerDefinitions[i]
		if !namePattern.MatchString(common.Value(c.Name)) || names[*c.Name] {
			return nil, clientError("Container names must be unique and up to 255 letters, numbers, hyphens, and " +
				"underscores.")
		}

		names[*c.Name] = true
	}

	essential := false
	out := make([]api.ContainerDefinition, 0, len(in.ContainerDefinitions))
	for i := range in.ContainerDefinitions {
		c := in.ContainerDefinitions[i]
		if err := checkContainer(in, &c, names, volumes); err != nil {
			return nil, err
		}

		if c.Essential == nil {
			c.Essential = new(true)
		}

		essential = essential || *c.Essential
		ports := make([]api.PortMapping, 0, len(c.PortMappings))
		for _, p := range c.PortMappings {
			if p.Protocol == nil {
				p.Protocol = new("tcp")
			}

			if p.HostPort == nil && common.Value(in.NetworkMode) != bridge && in.NetworkMode != nil {
				p.HostPort = p.ContainerPort
			}

			ports = append(ports, p)
		}

		if c.PortMappings != nil {
			c.PortMappings = ports
		}

		out = append(out, c)
	}

	if !essential {
		return nil, clientError("A task definition must have at least one essential container.")
	}

	return out, nil
}

func checkContainer(
	in *api.RegisterTaskDefinitionRequest,
	c *api.ContainerDefinition,
	names, volumes map[string]bool,
) error {
	name := *c.Name
	switch {
	case common.Value(c.Image) == "":
		return clientError("Container.image should not be null or empty.")
	case in.Memory == nil && c.Memory == nil && c.MemoryReservation == nil:
		return clientError("Invalid setting for container '%s'. At least one of 'memory' or 'memoryReservation' "+
			"must be specified.", name)
	case c.Memory != nil && c.MemoryReservation != nil && *c.MemoryReservation > *c.Memory:
		return clientError("Invalid setting for container '%s'. 'memory' must be greater than or equal to "+
			"'memoryReservation'.", name)
	case len(c.Links) > 0 && common.Value(in.NetworkMode) != bridge && in.NetworkMode != nil:
		return clientError("Links are only supported in bridge network mode.")
	case c.LogConfiguration != nil && !slices.Contains(logDrivers, common.Value(c.LogConfiguration.LogDriver)):
		return clientError("Log driver %s is not supported.", common.Value(c.LogConfiguration.LogDriver))
	}

	for _, p := range c.PortMappings {
		mode := common.Value(in.NetworkMode)
		switch {
		case common.Value(p.ContainerPort) < 1 || common.Value(p.ContainerPort) > 65535:
			return clientError("Invalid 'containerPort' setting for container '%s'.", name)
		case !oneOf(p.Protocol, "tcp", "udp"):
			return clientError("Port mapping protocols are tcp and udp.")
		case (mode == awsvpc || mode == host) && p.HostPort != nil && *p.HostPort != *p.ContainerPort:
			return clientError("When networkMode=%s, containerPort and hostPort must match.", mode)
		}
	}

	for _, m := range c.MountPoints {
		if !volumes[common.Value(m.SourceVolume)] {
			return clientError("Invalid 'mountPoints' setting for container '%s'. 'sourceVolume' must be a "+
				"volume of the task.", name)
		}
	}

	for _, d := range c.DependsOn {
		if !names[common.Value(d.ContainerName)] || !oneOf(d.Condition, "START", "COMPLETE", "SUCCESS", "HEALTHY") ||
			d.Condition == nil {
			return clientError("Invalid 'dependsOn' setting for container '%s'.", name)
		}
	}

	return nil
}

// described returns d's description, with its tags if asked to.
func (d *taskDef) described(include []string) ([]api.Tag, error) {
	for _, field := range include {
		if field != fieldTags {
			return nil, invalidParameter("include value must be TAGS.")
		}
	}

	if slices.Contains(include, fieldTags) {
		return append([]api.Tag{}, d.Tags...), nil
	}

	return nil, nil
}

func (s *Service) DescribeTaskDefinition(
	ctx context.Context,
	in *api.DescribeTaskDefinitionRequest,
) (*api.DescribeTaskDefinitionResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeTaskDefinitionResponse{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		d, resolveErr := resolveDef(tx, scope, in.TaskDefinition)
		if resolveErr != nil {
			return resolveErr
		}

		out.TaskDefinition = &d.Def
		out.Tags, resolveErr = d.described(in.Include)
		return resolveErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// revisionRef resolves ref, which must name a revision.
func revisionRef(tx storage.ReadTx, scope request.Scope, ref *string) (*taskDef, error) {
	name := common.Value(ref)
	if i := strings.LastIndex(name, "/"); strings.HasPrefix(name, "arn:") && i >= 0 {
		name = name[i+1:]
	}

	if !strings.Contains(name, ":") {
		return nil, clientError("The specified task definition does not exist; specify a family and revision.")
	}

	return resolveDef(tx, scope, ref)
}

func (s *Service) DeregisterTaskDefinition(
	ctx context.Context,
	in *api.DeregisterTaskDefinitionRequest,
) (*api.DeregisterTaskDefinitionResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.TaskDefinition
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		d, resolveErr := revisionRef(tx, scope, in.TaskDefinition)
		if resolveErr != nil {
			return resolveErr
		}

		if common.Value(d.Def.Status) == active {
			d.Def.Status = new(inactive)
			d.Def.DeregisteredAt = seconds(s.now().UTC())
		}

		out = &d.Def
		return storage.Write(tx, taskDefKey(scope, *d.Def.Family, *d.Def.Revision), d)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeregisterTaskDefinitionResponse{TaskDefinition: out}, nil
}

func (s *Service) ListTaskDefinitions(
	ctx context.Context,
	in *api.ListTaskDefinitionsRequest,
) (*api.ListTaskDefinitionsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if !oneOf(in.Status, active, inactive, deleteInProgress) || !oneOf(in.Sort, "ASC", "DESC") {
		return nil, invalidParameter("status must be ACTIVE, INACTIVE, or DELETE_IN_PROGRESS, and sort ASC or DESC.")
	}

	n, err := limit(in.MaxResults, 100)
	if err != nil {
		return nil, err
	}

	status := active
	if in.Status != nil {
		status = *in.Status
	}

	out := &api.ListTaskDefinitionsResponse{TaskDefinitionArns: []string{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		defs, readErr := storage.Records[taskDef](tx, scope.Prefix(service, "taskdef")+common.Value(in.FamilyPrefix))
		if readErr != nil {
			return readErr
		}

		defs = slices.DeleteFunc(defs, func(d *taskDef) bool { return common.Value(d.Def.Status) != status })
		if common.Value(in.Sort) == "DESC" {
			slices.Reverse(defs)
		}

		defs, out.NextToken, readErr = common.Page(defs, func(d *taskDef) string { return *d.Def.TaskDefinitionArn },
			in.NextToken, n, errPageToken)
		for _, d := range defs {
			out.TaskDefinitionArns = append(out.TaskDefinitionArns, *d.Def.TaskDefinitionArn)
		}

		return readErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) ListTaskDefinitionFamilies(
	ctx context.Context,
	in *api.ListTaskDefinitionFamiliesRequest,
) (*api.ListTaskDefinitionFamiliesResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if !oneOf(in.Status, active, inactive, statusAll) {
		return nil, invalidParameter("status must be ACTIVE, INACTIVE, or ALL.")
	}

	n, err := limit(in.MaxResults, 100)
	if err != nil {
		return nil, err
	}

	out := &api.ListTaskDefinitionFamiliesResponse{Families: []string{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		defs, readErr := storage.Records[taskDef](tx, scope.Prefix(service, "taskdef")+common.Value(in.FamilyPrefix))
		if readErr != nil {
			return readErr
		}

		// A family is active while one of its revisions is.
		var families []string
		isActive := map[string]bool{}
		for _, d := range defs {
			if !slices.Contains(families, *d.Def.Family) {
				families = append(families, *d.Def.Family)
			}

			isActive[*d.Def.Family] = isActive[*d.Def.Family] || common.Value(d.Def.Status) == active
		}

		families = slices.DeleteFunc(families, func(f string) bool {
			return common.Value(in.Status) == active && !isActive[f] ||
				common.Value(in.Status) == inactive && isActive[f]
		})
		families, out.NextToken, readErr = common.Page(
			families,
			func(f string) string { return f },
			in.NextToken,
			n,
			errPageToken,
		)
		out.Families = append(out.Families, families...)
		return readErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// DeleteTaskDefinitions deletes inactive revisions.
func (s *Service) DeleteTaskDefinitions(
	ctx context.Context,
	in *api.DeleteTaskDefinitionsRequest,
) (*api.DeleteTaskDefinitionsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if len(in.TaskDefinitions) < 1 || len(in.TaskDefinitions) > 10 {
		return nil, invalidParameter("taskDefinitions must name 1 to 10 task definitions.")
	}

	out := &api.DeleteTaskDefinitionsResponse{TaskDefinitions: []api.TaskDefinition{}, Failures: []api.Failure{}}
	now := s.now().UTC()
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, ref := range in.TaskDefinitions {
			d, resolveErr := revisionRef(tx, scope, &ref)
			switch {
			case resolveErr != nil:
				out.Failures = append(out.Failures, api.Failure{
					Arn: new(ref), Reason: new("TASK_DEFINITION_NOT_FOUND"), Detail: new(resolveErr.Error()),
				})
				continue
			case common.Value(d.Def.Status) == active:
				out.Failures = append(out.Failures, api.Failure{
					Arn: d.Def.TaskDefinitionArn, Reason: new("The specified task definition is still in ACTIVE " +
						"status. Please deregister the target and try again."),
				})
				continue
			}

			tx.Delete(taskDefKey(scope, *d.Def.Family, *d.Def.Revision))
			d.Def.Status, d.Def.DeleteRequestedAt = new(deleteInProgress), seconds(now)
			out.TaskDefinitions = append(out.TaskDefinitions, d.Def)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

package memorydb

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/memorydb"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	defaultPort = 6379
	// Sample values the MemoryDB guide shows when it picks the windows itself.
	defaultMaintenanceWindow = "wed:03:00-wed:04:00"
	defaultSnapshotWindow    = "04:30-05:30"
	parameterGroupInSync     = "in-sync"
	parameterApplying        = "applying"

	// The quotas page: "Shards per cluster can go up to 500"; the API: at most 5 replicas per shard.
	maxShards   = 500
	maxReplicas = 5

	// The Redis cluster keyspace is split into this many slots across the shards.
	hashSlots = 16384

	minutesPerDay  = 24 * 60
	minutesPerWeek = 7 * minutesPerDay
	// The model: "The minimum maintenance window is a 60 minute period."
	minMaintenanceMinutes = 60
)

// engineVersion is a supported engine, major.minor version and the default parameter group of that version. The
// order is the upgrade order: a cluster can move to a later entry, never to an earlier one.
type engineVersion struct {
	engine, version, patch, parameterGroup string
	preferred                              bool
}

// Sources: the engine-versions, parametergroups.redis and getting-started pages of the MemoryDB guide.
// MemoryDB offers one version per minor release, so only major.minor is accepted. A patch version is reported
// only where the guide states it.
var engineVersions = []engineVersion{
	{engine: engineRedis, version: "6.2", parameterGroup: "default.memorydb-redis6", preferred: true},
	{engine: engineRedis, version: redisVersion7, parameterGroup: "default.memorydb-redis7"},
	{engine: engineRedis, version: "7.1", parameterGroup: "default.memorydb-redis7"},
	{engine: engineValkey, version: "7.2", patch: "7.2.6", parameterGroup: "default.memorydb-valkey7", preferred: true},
}

const (
	engineRedis  = "redis"
	engineValkey = "valkey"

	// defaultEngine is used when neither an engine nor a version is given.
	defaultEngine = engineValkey
)

type clusterRecord struct {
	CreatedAt time.Time
	tagSet
	EngineVersion        string
	Engine               string
	Admin                string
	RuntimeEngine        string
	RuntimeVersion       string
	Container            string
	Name                 string
	Description          string
	NodeType             string
	EndpointToken        string
	RuntimeStatus        string
	ParameterGroupName   string
	ParameterGroupStatus string
	MaintenanceWindow    string
	SnapshotWindow       string
	SubnetGroupName      string
	KmsKeyID             string
	ACLName              string
	SecurityGroupIDs     []string
	ParameterRevision    uint64
	Port                 int
	NumShards            int
	NumReplicas          int
	HostPort             int64
	SnapshotRetention    int
	TLS                  bool
	AutoMinorUpgrade     bool
}

// clusterPatch holds the settings a request sets; nil leaves a setting as it is. Creating a cluster merges a
// patch into the defaults and updating merges one into the stored cluster, so both validate the same candidate.
type clusterPatch struct {
	Description, NodeType, ParameterGroupName, SubnetGroupName, MaintenanceWindow, SnapshotWindow *string
	KmsKeyID, ACLName, Engine, EngineVersion                                                      *string
	NumShards, NumReplicas, Port, SnapshotRetention                                               *int64
	TLS, AutoMinorUpgrade                                                                         *bool
	SecurityGroupIDs                                                                              []string
}

func clusterNotFound(name string) error {
	return fault("ClusterNotFoundFault", "Cluster %s not found.", name)
}

func set[T any](dst, src *T) {
	if src != nil {
		*dst = *src
	}
}

func setLower(dst, src *string) {
	if src != nil {
		*dst = strings.ToLower(*src)
	}
}

func setInt(dst *int, src *int64) {
	if src != nil {
		*dst = int(*src)
	}
}

// merge applies p to the cluster. The engine and its parameter group are resolved together: a changed engine
// without a version takes the new engine's preferred version, and the parameter group follows the version.
func (r *clusterRecord) merge(p *clusterPatch) error {
	set(&r.Description, p.Description)
	set(&r.NodeType, p.NodeType)
	set(&r.KmsKeyID, p.KmsKeyID)
	set(&r.TLS, p.TLS)
	set(&r.AutoMinorUpgrade, p.AutoMinorUpgrade)
	setInt(&r.NumShards, p.NumShards)
	setInt(&r.NumReplicas, p.NumReplicas)
	setInt(&r.Port, p.Port)
	setInt(&r.SnapshotRetention, p.SnapshotRetention)
	if p.SecurityGroupIDs != nil {
		r.SecurityGroupIDs = slices.Clone(p.SecurityGroupIDs)
	}

	// Names and the days of a maintenance window are lower-case.
	setLower(&r.SubnetGroupName, p.SubnetGroupName)
	setLower(&r.ACLName, p.ACLName)
	setLower(&r.MaintenanceWindow, p.MaintenanceWindow)

	set(&r.SnapshotWindow, p.SnapshotWindow)

	engine, version := common.Value(p.Engine), common.Value(p.EngineVersion)
	if engine == "" {
		engine = r.Engine
	}

	if version == "" && engine == r.Engine {
		version = r.EngineVersion
	}

	ev, err := findEngine(engine, version)
	if err != nil {
		return err
	}

	r.Engine, r.EngineVersion = ev.engine, ev.version
	if p.ParameterGroupName != nil {
		r.ParameterGroupName = normalize(p.ParameterGroupName)
	} else if r.ParameterGroupName == "" || strings.HasPrefix(r.ParameterGroupName, "default.") {
		r.ParameterGroupName = ev.parameterGroup
	}

	return nil
}

func findEngine(engine, version string) (engineVersion, error) {
	if engine != "" && !slices.ContainsFunc(engineVersions, func(e engineVersion) bool { return e.engine == engine }) {
		return engineVersion{}, invalidValue("Engine must be redis or valkey.")
	}

	for _, e := range engineVersions {
		switch {
		case engine != "" && e.engine != engine, version != "" && e.version != version:
		case version == "" && (!e.preferred || engine == "" && e.engine != defaultEngine):
		default:
			return e, nil
		}
	}

	return engineVersion{}, invalidValue(
		"EngineVersion %s is not supported for engine %s. Specify the major and minor version only, such as 7.2.",
		version, engine,
	)
}

// validate checks a whole candidate cluster against the documented limits and cross-field rules.
func (r *clusterRecord) validate() error {
	if r.NodeType == "" {
		return invalidValue("NodeType is required.")
	}

	if strings.Contains(r.NodeType, "r6gd") {
		return invalidCombination("Data tiering is required for r6gd node types and is not supported.")
	}

	if r.NumShards < 1 || r.NumShards > maxShards {
		return invalidValue("NumShards must be between 1 and %d.", maxShards)
	}

	if r.NumReplicas < 0 || r.NumReplicas > maxReplicas {
		return invalidValue("NumReplicasPerShard must be between 0 and %d.", maxReplicas)
	}

	if r.Port < 1 || r.Port > 65535 {
		return invalidValue("Port must be between 1 and 65535.")
	}

	if r.SnapshotRetention < 0 {
		return invalidValue("SnapshotRetentionLimit cannot be negative.")
	}

	if !validMaintenanceWindow(r.MaintenanceWindow) {
		return invalidValue("MaintenanceWindow must be a range of at least 60 minutes such as sun:23:00-mon:01:30.")
	}

	if !validSnapshotWindow(r.SnapshotWindow) {
		return invalidValue("SnapshotWindow must be a daily range such as 05:00-09:00.")
	}

	if slices.Contains(r.SecurityGroupIDs, "") {
		return invalidValue("SecurityGroupIds cannot contain empty values.")
	}

	if r.ACLName == "" {
		return invalidValue("ACLName is required.")
	}

	if !r.TLS && r.ACLName != openAccessACL {
		return invalidCombination("A cluster without TLS must use the %s ACL.", openAccessACL)
	}

	return nil
}

// clockMinutes reads hh24:mi as minutes since midnight.
func clockMinutes(s string) (int, bool) {
	var h, m int
	if len(s) != 5 || s[2] != ':' {
		return 0, false
	}

	if _, err := fmt.Sscanf(s, "%02d:%02d", &h, &m); err != nil || h > 23 || m > 59 {
		return 0, false
	}

	return h*60 + m, true
}

// weekMinutes reads ddd:hh24:mi as minutes since the start of the week, which begins on Sunday.
func weekMinutes(s string) (int, bool) {
	day, clock, ok := strings.Cut(s, ":")
	d := slices.Index([]string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}, day)
	m, valid := clockMinutes(clock)
	if !ok || d < 0 || !valid {
		return 0, false
	}

	return d*minutesPerDay + m, true
}

func validMaintenanceWindow(w string) bool {
	from, to, ok := strings.Cut(w, "-")
	start, startOK := weekMinutes(from)
	end, endOK := weekMinutes(to)
	return ok && startOK && endOK && (end-start+minutesPerWeek)%minutesPerWeek >= minMaintenanceMinutes
}

func validSnapshotWindow(w string) bool {
	from, to, ok := strings.Cut(w, "-")
	_, startOK := clockMinutes(from)
	_, endOK := clockMinutes(to)
	return ok && startOK && endOK
}

// checkReferences verifies the ACL and subnet group a cluster names.
func checkReferences(tx storage.ReadTx, scope request.Scope, r *clusterRecord) error {
	group, err := readParameterGroup(tx, scope, r.ParameterGroupName)
	if err != nil {
		return err
	}

	ev, err := findEngine(r.Engine, r.EngineVersion)
	if err != nil {
		return err
	}

	if defaultGroup(group.Family) != ev.parameterGroup {
		return invalidCombination(
			"Parameter group %s does not match engine %s %s.",
			r.ParameterGroupName,
			r.Engine,
			r.EngineVersion,
		)
	}

	if r.EngineVersion == redisVersion7 &&
		(group.Values["search-enabled"] == parameterYes || group.Values["search-query-timeout-ms"] != "") {
		return invalidCombination("Search parameters require engine version 7.1 or later.")
	}

	if _, err := readACL(tx, scope, r.ACLName); err != nil {
		return err
	}

	if r.SubnetGroupName != "" {
		if _, found := tx.Get(key(scope, kindSubnetGroup, r.SubnetGroupName)); !found {
			return subnetGroupNotFound(r.SubnetGroupName)
		}
	}

	return nil
}

// zones lists the Availability Zones a cluster's nodes spread over: those of its subnet group, or a single zone
// of the Region without one.
func zones(tx storage.ReadTx, scope request.Scope, r *clusterRecord) ([]string, error) {
	if r.SubnetGroupName == "" {
		return []string{scope.Region + "a"}, nil
	}

	g, _, err := storage.Read[subnetGroupRecord](tx, key(scope, kindSubnetGroup, r.SubnetGroupName))
	if err != nil {
		return nil, err
	}

	var out []string
	for _, sn := range g.Subnets {
		if !slices.Contains(out, sn.AvailabilityZone) {
			out = append(out, sn.AvailabilityZone)
		}
	}

	return out, nil
}

func (r *clusterRecord) output(scope request.Scope, zones []string, shards bool, status string) *api.Cluster {
	if r.RuntimeStatus != "" && status != statusDeleting {
		status = r.RuntimeStatus
	}

	patch := ""
	if i := slices.IndexFunc(engineVersions, func(e engineVersion) bool {
		return e.engine == r.Engine && e.version == r.EngineVersion
	}); i >= 0 {
		patch = engineVersions[i].patch
	}

	availability := "singleaz"
	if r.NumReplicas > 0 {
		availability = "multiaz"
	}

	out := &api.Cluster{
		Name:        new(r.Name),
		Description: optional(r.Description),
		Status:      new(status),
		ClusterEndpoint: &api.Endpoint{
			Address: new(fmt.Sprintf(
				"clustercfg.%s.%s.memorydb.%s.amazonaws.com", r.Name, r.EndpointToken, scope.Region,
			)),
			Port: new(int64(r.Port)),
		},
		NumberOfShards:          new(int64(r.NumShards)),
		AvailabilityMode:        new(availability),
		NodeType:                new(r.NodeType),
		Engine:                  new(r.Engine),
		EngineVersion:           new(r.EngineVersion),
		EnginePatchVersion:      optional(patch),
		ParameterGroupName:      new(r.ParameterGroupName),
		ParameterGroupStatus:    common.OrElse(optional(r.ParameterGroupStatus), new(parameterGroupInSync)),
		SubnetGroupName:         optional(r.SubnetGroupName),
		TLSEnabled:              new(r.TLS),
		KmsKeyId:                optional(r.KmsKeyID),
		ARN:                     new(arn(scope, kindCluster, r.Name)),
		SnapshotRetentionLimit:  new(int64(r.SnapshotRetention)),
		MaintenanceWindow:       new(r.MaintenanceWindow),
		SnapshotWindow:          new(r.SnapshotWindow),
		ACLName:                 new(r.ACLName),
		AutoMinorVersionUpgrade: new(r.AutoMinorUpgrade),
		SecurityGroups:          []api.SecurityGroupMembership{},
	}

	for _, id := range r.SecurityGroupIDs {
		out.SecurityGroups = append(out.SecurityGroups, api.SecurityGroupMembership{
			SecurityGroupId: new(id), Status: new(statusActive),
		})
	}

	if r.RuntimeStatus != "" {
		out.ClusterEndpoint = &api.Endpoint{Address: new("127.0.0.1"), Port: new(r.HostPort)}
	}

	if shards {
		out.Shards = r.shards(zones)
		if r.RuntimeStatus != "" {
			for i := range out.Shards {
				out.Shards[i].Status = new(status)
				for j := range out.Shards[i].Nodes {
					out.Shards[i].Nodes[j].Status = new(status)
					out.Shards[i].Nodes[j].Endpoint = out.ClusterEndpoint
				}
			}
		}
	}

	return out
}

// shards synthesizes the shards of a cluster: the keyspace is split evenly, the first node of a shard is its
// primary, and nodes rotate through the zones.
func (r *clusterRecord) shards(zones []string) []api.Shard {
	out := make([]api.Shard, 0, r.NumShards)
	slot := 0
	for i := range r.NumShards {
		size := hashSlots / r.NumShards
		if i < hashSlots%r.NumShards {
			size++
		}

		shard := api.Shard{
			Name:          new(fmt.Sprintf("%04d", i+1)),
			Status:        new(statusAvailable),
			Slots:         new(fmt.Sprintf("%d-%d", slot, slot+size-1)),
			NumberOfNodes: new(int64(r.NumReplicas + 1)),
		}

		slot += size
		for j := range r.NumReplicas + 1 {
			shard.Nodes = append(shard.Nodes, api.Node{
				Name:             new(fmt.Sprintf("%04d", j+1)),
				Status:           new(statusAvailable),
				AvailabilityZone: new(zones[(i+j)%len(zones)]),
				CreateTime:       new(common.Unix(r.CreatedAt)),
			})
		}

		out = append(out, shard)
	}

	return out
}

func (s *Service) CreateCluster(ctx context.Context, in *api.CreateClusterRequest) (*api.CreateClusterResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := normalize(in.ClusterName)
	err = checkName("ClusterName", name)
	if err != nil {
		return nil, err
	}

	if in.KmsKeyId != nil && *in.KmsKeyId == "" {
		return nil, invalidValue("KmsKeyId cannot be empty.")
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	r := &clusterRecord{
		Name:              name,
		CreatedAt:         time.Now().UTC().Truncate(time.Millisecond),
		tagSet:            tagSet{Tags: tags},
		EndpointToken:     common.RandomID(6),
		NumShards:         1,
		NumReplicas:       1,
		Port:              defaultPort,
		TLS:               true,
		AutoMinorUpgrade:  true,
		MaintenanceWindow: defaultMaintenanceWindow,
		SnapshotWindow:    defaultSnapshotWindow,
	}

	err = r.merge(&clusterPatch{
		Description: in.Description, NodeType: in.NodeType, ParameterGroupName: in.ParameterGroupName,
		SubnetGroupName: in.SubnetGroupName, MaintenanceWindow: in.MaintenanceWindow, SnapshotWindow: in.SnapshotWindow,
		KmsKeyID: in.KmsKeyId, ACLName: in.ACLName, Engine: in.Engine, EngineVersion: in.EngineVersion,
		NumShards: in.NumShards, NumReplicas: in.NumReplicasPerShard, Port: in.Port,
		SnapshotRetention: in.SnapshotRetentionLimit, TLS: in.TLSEnabled, AutoMinorUpgrade: in.AutoMinorVersionUpgrade,
		SecurityGroupIDs: in.SecurityGroupIds,
	})
	if err != nil {
		return nil, err
	}

	err = r.validate()
	if err != nil {
		return nil, err
	}

	if s.runtime != nil {
		if _, err = engineImage(r.Engine, r.EngineVersion); err != nil {
			return nil, err
		}

		r.RuntimeStatus, r.Admin = statusCreating, common.RandomID(40)
		r.RuntimeEngine, r.RuntimeVersion = r.Engine, r.EngineVersion
	}

	var out *api.Cluster
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, exists := tx.Get(key(scope, kindCluster, name)); exists {
			return fault("ClusterAlreadyExistsFault", "Cluster %s already exists.", name)
		}

		e := checkRuntimeACL(tx, scope, r, s.runtime != nil)
		if e != nil {
			return e
		}

		e = checkReferences(tx, scope, r)
		if e != nil {
			return e
		}

		az, e := zones(tx, scope, r)
		if e != nil {
			return e
		}

		if s.runtime != nil {
			r.ParameterGroupStatus = parameterApplying
			r.ParameterRevision++
		}

		out = r.output(scope, az, false, statusAvailable)
		if s.runtime != nil {
			if e := storage.Write(tx, indexKey(scope, name), true); e != nil {
				return e
			}
		}

		return storage.Write(tx, key(scope, kindCluster, name), r)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateClusterResponse{Cluster: out}, nil
}

func (s *Service) UpdateCluster(ctx context.Context, in *api.UpdateClusterRequest) (*api.UpdateClusterResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := normalize(in.ClusterName)
	patch := clusterPatch{
		Description:        in.Description,
		MaintenanceWindow:  in.MaintenanceWindow,
		ParameterGroupName: in.ParameterGroupName,
		ACLName:            in.ACLName,
		SnapshotWindow:     in.SnapshotWindow,
		SnapshotRetention:  in.SnapshotRetentionLimit,
		NodeType:           in.NodeType,
		Engine:             in.Engine,
		EngineVersion:      in.EngineVersion,
		SecurityGroupIDs:   in.SecurityGroupIds,
	}

	if in.ReplicaConfiguration != nil {
		if in.ReplicaConfiguration.ReplicaCount == nil {
			return nil, invalidValue("ReplicaConfiguration requires ReplicaCount.")
		}

		patch.NumReplicas = in.ReplicaConfiguration.ReplicaCount
	}

	if in.ShardConfiguration != nil {
		if in.ShardConfiguration.ShardCount == nil {
			return nil, invalidValue("ShardConfiguration requires ShardCount.")
		}

		patch.NumShards = in.ShardConfiguration.ShardCount
	}

	var out *api.Cluster
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, found, e := storage.Read[clusterRecord](tx, key(scope, kindCluster, name))
		if e != nil {
			return e
		}

		if !found {
			return clusterNotFound(name)
		}

		oldGroup, e := readParameterGroup(tx, scope, r.ParameterGroupName)
		if e != nil {
			return e
		}

		old := engineIndex(r.Engine, r.EngineVersion)
		e = r.merge(&patch)
		if e != nil {
			return e
		}

		if engineIndex(r.Engine, r.EngineVersion) < old {
			return invalidCombination("An engine cannot be downgraded to an earlier version.")
		}

		e = r.validate()
		if e != nil {
			return e
		}

		if aclErr := checkRuntimeACL(tx, scope, r, s.runtime != nil); aclErr != nil {
			return aclErr
		}

		e = checkReferences(tx, scope, r)
		if e != nil {
			return e
		}

		newGroup, e := readParameterGroup(tx, scope, r.ParameterGroupName)
		if e != nil {
			return e
		}

		if oldGroup.Name != newGroup.Name {
			for _, parameter := range []string{parameterActiveRehashing, "search-enabled"} {
				if conditionalParameter(
					oldGroup.Values,
					parameter,
				) != conditionalParameter(
					newGroup.Values,
					parameter,
				) {
					return invalidCombination("%s must match when changing groups.", parameter)
				}
			}
		}

		az, e := zones(tx, scope, r)
		if e != nil {
			return e
		}

		if s.runtime != nil {
			r.ParameterGroupStatus = parameterApplying
			r.ParameterRevision++
		}

		out = r.output(scope, az, false, statusAvailable)
		return storage.Write(tx, key(scope, kindCluster, name), r)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateClusterResponse{Cluster: out}, nil
}

func engineIndex(engine, version string) int {
	return slices.IndexFunc(
		engineVersions,
		func(e engineVersion) bool { return e.engine == engine && e.version == version },
	)
}

func (s *Service) DeleteCluster(ctx context.Context, in *api.DeleteClusterRequest) (*api.DeleteClusterResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := normalize(in.ClusterName)
	var out *api.Cluster
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, found, e := storage.Read[clusterRecord](tx, key(scope, kindCluster, name))
		if e != nil {
			return e
		}

		if !found {
			return clusterNotFound(name)
		}

		az, e := zones(tx, scope, r)
		if e != nil {
			return e
		}

		out = r.output(scope, az, false, statusDeleting)
		if s.runtime != nil {
			r.RuntimeStatus = statusDeleting
			return storage.Write(tx, key(scope, kindCluster, name), r)
		}

		tx.Delete(key(scope, kindCluster, name))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteClusterResponse{Cluster: out}, nil
}

func (s *Service) DescribeClusters(
	ctx context.Context,
	in *api.DescribeClustersRequest,
) (*api.DescribeClustersResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	limit, err := pageSize(in.MaxResults)
	if err != nil {
		return nil, err
	}

	var items []*clusterRecord
	zoneNames := map[string][]string{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		if in.ClusterName == nil {
			items, e = storage.Records[clusterRecord](tx, scope.Prefix(serviceName, kindCluster))
		} else {
			name := normalize(in.ClusterName)
			r, found, readErr := storage.Read[clusterRecord](tx, key(scope, kindCluster, name))
			switch {
			case readErr != nil:
				e = readErr
			case !found:
				e = clusterNotFound(name)
			default:
				items = []*clusterRecord{r}
			}
		}

		if e != nil {
			return e
		}

		for _, r := range items {
			if zoneNames[r.Name], e = zones(tx, scope, r); e != nil {
				return e
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		items, func(r *clusterRecord) string { return r.Name }, in.NextToken, limit, invalidToken(),
	)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeClustersResponse{NextToken: next, Clusters: []api.Cluster{}}
	for _, r := range page {
		out.Clusters = append(
			out.Clusters,
			*r.output(scope, zoneNames[r.Name], common.Value(in.ShowShardDetails), statusAvailable),
		)
	}

	return out, nil
}

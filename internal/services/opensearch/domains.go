package opensearch

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/opensearch"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) CreateDomain(
	ctx context.Context,
	in *api.CreateDomainRequest,
) (*api.CreateDomainResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.DomainName == nil || *in.DomainName == "" {
		return nil, invalid("DomainName is required")
	}

	name := *in.DomainName
	if nameErr := validateDomainName(name); nameErr != nil {
		return nil, nameErr
	}

	engineVer := defaultEngineVer
	if in.EngineVersion != nil && *in.EngineVersion != "" {
		if verErr := validateEngineVersion(*in.EngineVersion); verErr != nil {
			return nil, verErr
		}

		engineVer = *in.EngineVersion
	}

	if in.AccessPolicies != nil && *in.AccessPolicies != "" {
		if polErr := validateAccessPolicies(*in.AccessPolicies); polErr != nil {
			return nil, polErr
		}
	}

	if s.runtime != nil {
		if len(in.AdvancedOptions) > 0 {
			return nil, unsupported("Custom advanced options are not supported in Docker mode")
		}

		if _, engineErr := engineSpec(engineVer); engineErr != nil {
			return nil, engineErr
		}
	}

	tags, err := tagMap(in.TagList)
	if err != nil {
		return nil, err
	}

	if tagErr := tagging.CheckSet(tags); tagErr != nil {
		return nil, checkTagLimit(tagErr)
	}

	key := scope.Key(serviceName, domainKind, name)
	arn := domainARN(scope.Region, scope.AccountID, name)
	domainID := scope.AccountID + "/" + name
	now := time.Now().UTC()

	status := &api.DomainStatus{
		ARN:                    new(arn),
		DomainId:               new(domainID),
		DomainName:             new(name),
		Created:                new(true),
		Deleted:                new(false),
		Processing:             new(false),
		EngineVersion:          new(engineVer),
		AccessPolicies:         in.AccessPolicies,
		AdvancedOptions:        defaultAdvancedOptions(),
		DomainProcessingStatus: new("Active"),
	}

	if s.runtime == nil {
		status.Endpoint = new(domainEndpoint(name, scope.Region))
	} else {
		// The engine's loopback port is known once its container runs; the domain processes until then.
		status.Processing = new(true)
		status.DomainProcessingStatus = new("Creating")
	}

	if changeErr := applyAdvanced(status, in.AdvancedOptions); changeErr != nil {
		return nil, changeErr
	}

	base := baseDomain()
	status.ClusterConfig = base.ClusterConfig
	status.EBSOptions = base.EBSOptions
	status.EncryptionAtRestOptions = base.EncryptionAtRestOptions
	status.NodeToNodeEncryptionOptions = base.NodeToNodeEncryptionOptions
	status.DomainEndpointOptions = base.DomainEndpointOptions
	merge(status, domainOptions{
		cluster:    in.ClusterConfig,
		ebs:        in.EBSOptions,
		atRest:     in.EncryptionAtRestOptions,
		nodeToNode: in.NodeToNodeEncryptionOptions,
		endpoint:   in.DomainEndpointOptions,
	})
	if checkErr := checkDomain(status, nil); checkErr != nil {
		return nil, checkErr
	}

	rec := domainRecord{
		Status:        status,
		Tags:          tags,
		CreatedAt:     now,
		UpdatedAt:     now,
		Since:         now,
		UpdateVersion: 1,
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		count := 0
		for _, k := range tx.Keys(scope.Prefix(serviceName, domainKind)) {
			if _, _, _, ok := storage.Scoped(k, serviceName); ok {
				count++
			}
		}

		if count >= maxDomains {
			return limitExceeded("Domain limit of %d exceeded", maxDomains)
		}

		_, found, readErr := storage.Read[domainRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if found {
			return alreadyExists("Domain %s already exists", name)
		}

		if s.runtime != nil {
			if writeErr := storage.Write(tx, indexKey(scope, name), true); writeErr != nil {
				return writeErr
			}
		}

		return storage.Write(tx, key, rec)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateDomainResponse{DomainStatus: common.Clone(rec.Status)}, nil
}

func (s *Service) DescribeDomain(
	ctx context.Context,
	in *api.DescribeDomainRequest,
) (*api.DescribeDomainResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.DomainName == nil || *in.DomainName == "" {
		return nil, invalid("DomainName is required")
	}

	var rec *domainRecord
	key := scope.Key(serviceName, domainKind, *in.DomainName)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, found, readErr := storage.Read[domainRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found {
			return notFound("Domain not found: %s", *in.DomainName)
		}

		if changeErr := settle(tx, key, r); changeErr != nil {
			return changeErr
		}

		rec = r
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeDomainResponse{DomainStatus: common.Clone(rec.Status)}, nil
}

func (s *Service) DescribeDomains(
	ctx context.Context,
	in *api.DescribeDomainsRequest,
) (*api.DescribeDomainsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || len(in.DomainNames) == 0 || len(in.DomainNames) > 5 {
		return nil, invalid("DescribeDomains takes between 1 and 5 domain names")
	}

	var list []api.DomainStatus
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, name := range in.DomainNames {
			key := scope.Key(serviceName, domainKind, name)
			r, found, readErr := storage.Read[domainRecord](tx, key)
			if readErr != nil {
				return readErr
			}

			if found && r != nil && r.Status != nil {
				if changeErr := settle(tx, key, r); changeErr != nil {
					return changeErr
				}

				list = append(list, *common.Clone(r.Status))
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeDomainsResponse{DomainStatusList: list}, nil
}

func (s *Service) DescribeDomainConfig(
	ctx context.Context,
	in *api.DescribeDomainConfigRequest,
) (*api.DescribeDomainConfigResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.DomainName == nil || *in.DomainName == "" {
		return nil, invalid("DomainName is required")
	}

	var rec *domainRecord
	key := scope.Key(serviceName, domainKind, *in.DomainName)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, found, readErr := storage.Read[domainRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found {
			return notFound("Domain not found: %s", *in.DomainName)
		}

		if changeErr := settle(tx, key, r); changeErr != nil {
			return changeErr
		}

		rec = r
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeDomainConfigResponse{DomainConfig: domainConfigFromRecord(rec)}, nil
}

func (s *Service) ListDomainNames(
	ctx context.Context,
	in *api.ListDomainNamesRequest,
) (*api.ListDomainNamesResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in != nil && in.EngineType != nil && *in.EngineType != "" {
		if *in.EngineType != engineOpenSearch && *in.EngineType != engineElasticsearch {
			return nil, invalid("Invalid EngineType: %s. Must be OpenSearch or Elasticsearch", *in.EngineType)
		}
	}

	var infos []api.DomainInfo
	prefix := scope.Prefix(serviceName, domainKind)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, key := range tx.Keys(prefix) {
			rec, found, readErr := storage.Read[domainRecord](tx, key)
			if readErr != nil {
				return readErr
			}

			if !found || rec == nil || rec.Status == nil || rec.Status.DomainName == nil {
				continue
			}

			if settleErr := settle(tx, key, rec); settleErr != nil {
				return settleErr
			}

			engineType := engineOpenSearch
			if rec.Status.EngineVersion != nil && strings.HasPrefix(*rec.Status.EngineVersion, engineElasticsearch) {
				engineType = engineElasticsearch
			}

			if in != nil && in.EngineType != nil && *in.EngineType != "" && *in.EngineType != engineType {
				continue
			}

			infos = append(infos, api.DomainInfo{
				DomainName: rec.Status.DomainName,
				EngineType: new(engineType),
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	slices.SortFunc(infos, func(a, b api.DomainInfo) int {
		return strings.Compare(common.Value(a.DomainName), common.Value(b.DomainName))
	})

	return &api.ListDomainNamesResponse{DomainNames: infos}, nil
}

func (s *Service) UpdateDomainConfig(
	ctx context.Context,
	in *api.UpdateDomainConfigRequest,
) (*api.UpdateDomainConfigResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.DomainName == nil || *in.DomainName == "" {
		return nil, invalid("DomainName is required")
	}

	if in.AccessPolicies != nil && *in.AccessPolicies != "" {
		if polErr := validateAccessPolicies(*in.AccessPolicies); polErr != nil {
			return nil, polErr
		}
	}

	if s.runtime != nil && len(in.AdvancedOptions) > 0 {
		return nil, unsupported("Custom advanced options are not supported in Docker mode")
	}

	key := scope.Key(serviceName, domainKind, *in.DomainName)
	var updatedRec domainRecord
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, found, readErr := storage.Read[domainRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found || rec == nil {
			return notFound("Domain not found: %s", *in.DomainName)
		}

		if common.Value(rec.Status.Deleted) {
			return invalid("Domain %s is being deleted", *in.DomainName)
		}

		if changeErr := settle(tx, key, rec); changeErr != nil {
			return changeErr
		}

		if !rec.PendingUntil.IsZero() || common.Value(rec.Status.Processing) {
			return invalid("Domain is processing another change")
		}

		candidate := *common.Clone(rec)
		candidate.Status = common.Clone(rec.Status)
		candidate.Tags = common.CloneTags(rec.Tags)
		candidate.UpdatedAt = time.Now().UTC()
		if !candidate.UpdatedAt.After(rec.UpdatedAt.Add(time.Millisecond)) {
			candidate.UpdatedAt = rec.UpdatedAt.Add(time.Millisecond)
		}

		candidate.UpdateVersion = rec.UpdateVersion + 1

		if in.AccessPolicies != nil {
			candidate.Status.AccessPolicies = in.AccessPolicies
		}

		merge(candidate.Status, domainOptions{
			cluster:    in.ClusterConfig,
			ebs:        in.EBSOptions,
			atRest:     in.EncryptionAtRestOptions,
			nodeToNode: in.NodeToNodeEncryptionOptions,
			endpoint:   in.DomainEndpointOptions,
		})
		if checkErr := checkDomain(candidate.Status, rec.Status); checkErr != nil {
			return checkErr
		}

		if changeErr := applyAdvanced(candidate.Status, in.AdvancedOptions); changeErr != nil {
			return changeErr
		}
		beginChange(&candidate, rec.Status)
		updatedRec = candidate
		return storage.Write(tx, key, candidate)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateDomainConfigResponse{DomainConfig: domainConfigFromRecord(&updatedRec)}, nil
}

func (s *Service) DeleteDomain(
	ctx context.Context,
	in *api.DeleteDomainRequest,
) (*api.DeleteDomainResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.DomainName == nil || *in.DomainName == "" {
		return nil, invalid("DomainName is required")
	}

	key := scope.Key(serviceName, domainKind, *in.DomainName)
	var deletedStatus *api.DomainStatus
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, found, readErr := storage.Read[domainRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found || rec == nil {
			return notFound("Domain not found: %s", *in.DomainName)
		}

		if s.runtime == nil {
			tx.Delete(key)

			deletedStatus = common.Clone(rec.Status)
			deletedStatus.Deleted = new(true)
			deletedStatus.Processing = new(false)
			return nil
		}

		// The domain processes until its container and volume are gone, and keeps its name until then.
		rec.PendingUntil = time.Time{}
		rec.Status.Deleted, rec.Status.Processing = new(true), new(true)
		rec.Status.DomainProcessingStatus = new("Deleting")
		deletedStatus = common.Clone(rec.Status)
		if writeErr := storage.Write(tx, indexKey(scope, *in.DomainName), true); writeErr != nil {
			return writeErr
		}

		return storage.Write(tx, key, *rec)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteDomainResponse{DomainStatus: deletedStatus}, nil
}

func validateAccessPolicies(policy string) error {
	var js any
	if err := json.Unmarshal([]byte(policy), &js); err != nil {
		return invalid("Invalid Access Policies: %v", err)
	}

	return nil
}

// baseDomain holds the configuration a new domain starts from before the request's options are merged.
func baseDomain() *api.DomainStatus {
	return &api.DomainStatus{
		ClusterConfig: &api.ClusterConfig{
			InstanceType:           new(defaultInstanceType),
			InstanceCount:          new(int64(1)),
			DedicatedMasterEnabled: new(false),
			ZoneAwarenessEnabled:   new(false),
		},
		EBSOptions: &api.EBSOptions{
			EBSEnabled: new(true),
			VolumeType: new("gp2"),
			VolumeSize: new(int64(10)),
		},
		EncryptionAtRestOptions:     &api.EncryptionAtRestOptions{Enabled: new(false)},
		NodeToNodeEncryptionOptions: &api.NodeToNodeEncryptionOptions{Enabled: new(false)},
		DomainEndpointOptions: &api.DomainEndpointOptions{
			EnforceHTTPS:      new(false),
			TLSSecurityPolicy: new(defaultTLSPolicy),
		},
	}
}

type domainOptions struct {
	cluster    *api.ClusterConfig
	ebs        *api.EBSOptions
	atRest     *api.EncryptionAtRestOptions
	nodeToNode *api.NodeToNodeEncryptionOptions
	endpoint   *api.DomainEndpointOptions
}

// merge applies the supplied option members over status; omitted members keep their current values.
func merge(status *api.DomainStatus, o domainOptions) {
	if c := o.cluster; c != nil {
		cur := status.ClusterConfig
		if c.DedicatedMasterEnabled != nil && !*c.DedicatedMasterEnabled {
			// Turning dedicated masters off drops their settings unless the request supplies them again.
			cur.DedicatedMasterType, cur.DedicatedMasterCount = nil, nil
		}

		cur.InstanceType = common.OrElse(c.InstanceType, cur.InstanceType)
		cur.InstanceCount = common.OrElse(c.InstanceCount, cur.InstanceCount)
		cur.DedicatedMasterEnabled = common.OrElse(c.DedicatedMasterEnabled, cur.DedicatedMasterEnabled)
		cur.DedicatedMasterType = common.OrElse(c.DedicatedMasterType, cur.DedicatedMasterType)
		cur.DedicatedMasterCount = common.OrElse(c.DedicatedMasterCount, cur.DedicatedMasterCount)
		cur.ZoneAwarenessEnabled = common.OrElse(c.ZoneAwarenessEnabled, cur.ZoneAwarenessEnabled)
	}

	if e := o.ebs; e != nil {
		if e.EBSEnabled != nil && !*e.EBSEnabled {
			// Instance-store domains have no volume settings; drop the previous ones.
			status.EBSOptions = &api.EBSOptions{}
		}

		cur := status.EBSOptions
		cur.EBSEnabled = common.OrElse(e.EBSEnabled, cur.EBSEnabled)
		previousType := common.Value(cur.VolumeType)
		cur.VolumeType = common.OrElse(e.VolumeType, cur.VolumeType)
		cur.VolumeSize = common.OrElse(e.VolumeSize, cur.VolumeSize)
		if e.VolumeType != nil && *e.VolumeType != previousType {
			cur.Iops, cur.Throughput = nil, nil
		}

		cur.Iops = common.OrElse(e.Iops, cur.Iops)
		cur.Throughput = common.OrElse(e.Throughput, cur.Throughput)
		if common.Value(cur.Iops) == 0 {
			cur.Iops = nil
		}

		if common.Value(cur.Throughput) == 0 {
			cur.Throughput = nil
		}
	}

	if e := o.atRest; e != nil {
		status.EncryptionAtRestOptions.Enabled = common.OrElse(e.Enabled, status.EncryptionAtRestOptions.Enabled)
		status.EncryptionAtRestOptions.KmsKeyId = common.OrElse(e.KmsKeyId, status.EncryptionAtRestOptions.KmsKeyId)
	}

	if e := o.nodeToNode; e != nil {
		status.NodeToNodeEncryptionOptions.Enabled = common.OrElse(
			e.Enabled,
			status.NodeToNodeEncryptionOptions.Enabled,
		)
	}

	if e := o.endpoint; e != nil {
		cur := status.DomainEndpointOptions
		cur.EnforceHTTPS = common.OrElse(e.EnforceHTTPS, cur.EnforceHTTPS)
		cur.TLSSecurityPolicy = common.OrElse(e.TLSSecurityPolicy, cur.TLSSecurityPolicy)
		cur.CustomEndpointEnabled = common.OrElse(e.CustomEndpointEnabled, cur.CustomEndpointEnabled)
		cur.CustomEndpoint = common.OrElse(e.CustomEndpoint, cur.CustomEndpoint)
	}
}

const volumeGP3 = "gp3"

var (
	volumeTypes = []string{"standard", "gp2", "io1", volumeGP3}
	tlsPolicies = []string{
		"Policy-Min-TLS-1-0-2019-07",
		"Policy-Min-TLS-1-2-2019-07",
		"Policy-Min-TLS-1-2-PFS-2023-10",
		"Policy-Min-TLS-1-2-RFC9151-FIPS-2024-08",
	}
)

// checkDomain validates a complete candidate configuration. previous is nil on create.
func checkDomain(status, previous *api.DomainStatus) error {
	c := status.ClusterConfig
	if !strings.HasSuffix(common.Value(c.InstanceType), ".search") {
		return invalid("InstanceType must be an OpenSearch Service instance type such as m5.large.search")
	}

	if common.Value(c.InstanceCount) < 1 {
		return invalid("InstanceCount must be at least 1")
	}

	if common.Value(c.ZoneAwarenessEnabled) && common.Value(c.InstanceCount)%2 != 0 {
		return invalid("A two Availability Zone deployment requires an even InstanceCount")
	}

	if common.Value(c.DedicatedMasterEnabled) {
		if !strings.HasSuffix(common.Value(c.DedicatedMasterType), ".search") ||
			common.Value(c.DedicatedMasterCount) < 1 {
			return invalid(
				"DedicatedMasterType and DedicatedMasterCount are required when dedicated masters are enabled",
			)
		}
	} else if c.DedicatedMasterType != nil || c.DedicatedMasterCount != nil {
		return invalid("DedicatedMasterType and DedicatedMasterCount require DedicatedMasterEnabled")
	}

	if e := status.EBSOptions; common.Value(e.EBSEnabled) {
		volume := common.Value(e.VolumeType)
		if !slices.Contains(volumeTypes, volume) {
			return invalid("VolumeType must be one of %s", strings.Join(volumeTypes, ", "))
		}

		if common.Value(e.VolumeSize) < 10 {
			return invalid("VolumeSize must be at least 10 GiB")
		}

		if e.Iops != nil && volume != "io1" && volume != volumeGP3 {
			return invalid("Iops applies only to io1 and gp3 volumes")
		}

		if e.Throughput != nil && volume != volumeGP3 {
			return invalid("Throughput applies only to gp3 volumes")
		}
	} else if e.VolumeSize != nil || e.Iops != nil || e.Throughput != nil {
		return invalid("Volume options require EBSEnabled")
	}

	atRest := status.EncryptionAtRestOptions
	if atRest.KmsKeyId != nil &&
		(!common.Value(atRest.Enabled) || len(*atRest.KmsKeyId) > 500 || *atRest.KmsKeyId == "") {
		return invalid("KmsKeyId requires encryption at rest and must contain 1-500 characters")
	}

	if previous != nil {
		if common.Value(previous.EncryptionAtRestOptions.Enabled) && !common.Value(atRest.Enabled) {
			return invalid("Encryption at rest cannot be disabled once enabled")
		}

		if common.Value(previous.NodeToNodeEncryptionOptions.Enabled) &&
			!common.Value(status.NodeToNodeEncryptionOptions.Enabled) {
			return invalid("Node-to-node encryption cannot be disabled once enabled")
		}
	}

	endpoint := status.DomainEndpointOptions
	if !slices.Contains(tlsPolicies, common.Value(endpoint.TLSSecurityPolicy)) {
		return invalid("TLSSecurityPolicy must be one of %s", strings.Join(tlsPolicies, ", "))
	}

	// Custom endpoints need a certificate ARN, which this slice does not support.
	if common.Value(endpoint.CustomEndpointEnabled) || endpoint.CustomEndpoint != nil {
		return invalid("Custom endpoints are not supported")
	}

	return nil
}

func defaultOptionStatus(created, updated time.Time, version int64) *api.OptionStatus {
	return &api.OptionStatus{
		CreationDate:    common.SecondsMilli(created),
		UpdateDate:      common.SecondsMilli(updated),
		UpdateVersion:   new(version),
		State:           new("Active"),
		PendingDeletion: new(false),
	}
}

func domainConfigFromRecord(rec *domainRecord) *api.DomainConfig {
	status := func(name string) *api.OptionStatus {
		if st := rec.OptionStatuses[name]; st != nil {
			return st
		}

		st := defaultOptionStatus(rec.CreatedAt, rec.CreatedAt, 1)
		if common.Value(rec.Status.Processing) && rec.PendingUntil.IsZero() {
			st.State = new("Processing")
		}

		return st
	}

	cfg := &api.DomainConfig{
		AdvancedOptions: &api.AdvancedOptionsStatus{
			Options: rec.Status.AdvancedOptions,
			Status:  status("AdvancedOptions"),
		},
		EngineVersion: &api.VersionStatus{
			Options: rec.Status.EngineVersion,
			Status:  status("EngineVersion"),
		},
		ClusterConfig: &api.ClusterConfigStatus{
			Options: rec.Status.ClusterConfig,
			Status:  status("ClusterConfig"),
		},
		EBSOptions: &api.EBSOptionsStatus{
			Options: rec.Status.EBSOptions,
			Status:  status("EBSOptions"),
		},
		AccessPolicies: &api.AccessPoliciesStatus{
			Options: rec.Status.AccessPolicies,
			Status:  status("AccessPolicies"),
		},
		EncryptionAtRestOptions: &api.EncryptionAtRestOptionsStatus{
			Options: rec.Status.EncryptionAtRestOptions,
			Status:  status("EncryptionAtRestOptions"),
		},
		NodeToNodeEncryptionOptions: &api.NodeToNodeEncryptionOptionsStatus{
			Options: rec.Status.NodeToNodeEncryptionOptions,
			Status:  status("NodeToNodeEncryptionOptions"),
		},
		DomainEndpointOptions: &api.DomainEndpointOptionsStatus{
			Options: rec.Status.DomainEndpointOptions,
			Status:  status("DomainEndpointOptions"),
		},
	}

	if common.Value(rec.Status.UpgradeProcessing) && !rec.UpgradeCheck {
		cfg.EngineVersion.Options = new(rec.UpgradeTarget)
	}

	return cfg
}

// beginChange records only changed options; unrelated option dates and versions stay stable.
func beginChange(rec *domainRecord, previous *api.DomainStatus) {
	if rec.OptionStatuses == nil {
		rec.OptionStatuses = map[string]*api.OptionStatus{}
	}

	before, after := reflect.ValueOf(previous).Elem(), reflect.ValueOf(rec.Status).Elem()
	for _, name := range []string{
		"EngineVersion", "ClusterConfig", "EBSOptions", "AccessPolicies", "AdvancedOptions",
		"EncryptionAtRestOptions", "NodeToNodeEncryptionOptions", "DomainEndpointOptions",
	} {
		old, pending := before.FieldByName(name).Interface(), after.FieldByName(name).Interface()
		if reflect.DeepEqual(old, pending) {
			continue
		}

		st := rec.OptionStatuses[name]
		if st == nil {
			st = defaultOptionStatus(rec.CreatedAt, rec.CreatedAt, 1)
		}

		st.UpdateVersion = new(common.Value(st.UpdateVersion) + 1)
		st.UpdateDate, st.State = common.SecondsMilli(rec.UpdatedAt), new("Processing")
		rec.OptionStatuses[name] = st
		activeJSON, _ := json.Marshal(old)
		pendingJSON, _ := json.Marshal(pending)
		rec.Status.ModifyingProperties = append(rec.Status.ModifyingProperties, api.ModifyingProperties{
			Name: new(
				name,
			),
			ActiveValue:  new(string(activeJSON)),
			PendingValue: new(string(pendingJSON)),
			ValueType:    new("PLAIN_TEXT"),
		})
	}

	if len(rec.Status.ModifyingProperties) > 0 {
		rec.PendingUntil = rec.UpdatedAt.Add(time.Second)
		rec.Status.Processing, rec.Status.DomainProcessingStatus = new(true), new("Modifying")
	}
}

// settle persists completion on the next observation, including after a process restart.
func settle(tx storage.WriteTx, key string, rec *domainRecord) error {
	if rec.PendingUntil.IsZero() || time.Now().Before(rec.PendingUntil) {
		return nil
	}

	rec.PendingUntil = time.Time{}
	rec.Status.Processing, rec.Status.UpgradeProcessing = new(false), new(false)
	rec.Status.DomainProcessingStatus = new("Active")
	rec.Status.ModifyingProperties = nil
	for _, st := range rec.OptionStatuses {
		st.State = new("Active")
	}

	if len(rec.History) > 0 && common.Value(rec.History[len(rec.History)-1].UpgradeStatus) == "IN_PROGRESS" {
		h := &rec.History[len(rec.History)-1]
		h.UpgradeStatus = new("SUCCEEDED")
		h.StepsList[0].UpgradeStepStatus, h.StepsList[0].ProgressPercent = new("SUCCEEDED"), new(float64(100))
		if !rec.UpgradeCheck {
			rec.Status.EngineVersion = new(rec.UpgradeTarget)
		}
	}

	return storage.Write(tx, key, *rec)
}

func defaultAdvancedOptions() map[string]string {
	return map[string]string{
		"override_main_response_version":         "false",
		"rest.action.multi_allow_explicit_index": "true",
		"indices.fielddata.cache.size":           "20",
		"indices.query.bool.max_clause_count":    "1024",
	}
}

func applyAdvanced(status *api.DomainStatus, options map[string]string) error {
	for k, v := range options {
		switch k {
		case "override_main_response_version", "rest.action.multi_allow_explicit_index":
			if v != "true" && v != "false" {
				return invalid("%s must be true or false", k)
			}
		case "indices.fielddata.cache.size", "indices.query.bool.max_clause_count":
			if n, err := strconv.Atoi(v); err != nil || n < 0 {
				return invalid("%s must be a nonnegative integer", k)
			}
		default:
			return unsupported("Advanced option %s is not supported", k)
		}

		status.AdvancedOptions[k] = v
	}

	return nil
}

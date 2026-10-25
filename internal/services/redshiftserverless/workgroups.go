package redshiftserverless

import (
	"context"
	"fmt"
	"slices"
	"time"
	"uuid"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/redshiftserverless"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) defaultSubnets(ctx context.Context, ids []string) ([]string, error) {
	if len(ids) > 0 {
		return ids, nil
	}

	out, err := s.caller.Call(
		ctx,
		"ec2",
		"DescribeSubnets",
		map[string]any{"Filters": []any{map[string]any{"Name": "default-for-az", "Values": []any{"true"}}}},
	)
	if err != nil {
		return nil, err
	}

	rows, _ := out["Subnets"].([]any)
	for _, item := range rows {
		sn, _ := item.(map[string]any)
		id, _ := sn["SubnetId"].(string)
		ids = append(ids, id)
	}

	if len(ids) == 0 {
		return nil, invalid("No default VPC subnets exist")
	}

	return ids, nil
}

func (s *Service) networkReferences(
	tx storage.ReadTx,
	scope request.Scope,
	ids, groups []string,
) (vpcID string, groupIDs []string, resultErr error) {
	vpc := ""
	for _, id := range ids {
		sn, err := s.network.MountSubnet(tx, scope, id)
		if err != nil {
			return "", nil, invalid("Subnet must exist: " + id)
		}

		if vpc != "" && vpc != common.Value(sn.VpcId) {
			return "", nil, invalid("Subnets must share a VPC")
		}

		vpc = common.Value(sn.VpcId)
	}

	if vpc == "" {
		return "", nil, invalid("Subnets are required")
	}

	resolved, err := s.network.MountGroups(tx, scope, vpc, groups)
	if err != nil {
		return "", nil, invalid("Security groups must exist in the subnet VPC")
	}

	return vpc, resolved, nil
}

func validateWorkgroup(w *api.Workgroup) error {
	port := common.Value(w.Port)
	if (port < 5431 || port > 5455) && (port < 8191 || port > 8215) {
		return invalid("Port must be in 5431-5455 or 8191-8215")
	}

	base := common.Value(w.BaseCapacity)
	if base < 4 || base > 1024 || (base < 8 && base != 4) || (base >= 8 && base%8 != 0) ||
		(base > 512 && base%32 != 0) {
		return invalid("Invalid baseCapacity in RPUs")
	}

	if w.MaxCapacity != nil && *w.MaxCapacity != -1 && (*w.MaxCapacity < 4 || *w.MaxCapacity > 5632) {
		return invalid("maxCapacity must be -1 (unlimited) or 4-5632 RPUs")
	}

	keys := []string{
		"auto_mv",
		"datestyle",
		"enable_case_sensitive_identifier",
		"enable_user_activity_logging",
		"query_group",
		"search_path",
		"require_ssl",
		"use_fips_ssl",
		"wlm_json_configuration",
		"max_scan_row_count",
		"max_query_execution_time",
		"max_query_queue_time",
		"max_query_blocks_read",
		"max_query_temp_blocks_to_disk",
		"max_join_row_count",
		"max_nested_loop_join_row_count",
		"max_io_skew",
		"max_query_cpu_usage_percent",
	}

	seen := map[string]bool{}

	for _, p := range w.ConfigParameters {
		k := common.Value(p.ParameterKey)
		if !slices.Contains(keys, k) || seen[k] {
			return invalid("Unsupported or duplicate config parameter: " + k)
		}

		seen[k] = true
	}

	if seen["wlm_json_configuration"] {
		for k := range seen {
			if len(k) > 4 && k[:4] == "max_" {
				return invalid("wlm_json_configuration conflicts with query monitoring metrics")
			}
		}
	}

	return nil
}

func (s *Service) CreateWorkgroup(
	ctx context.Context,
	in *api.CreateWorkgroupRequest,
) (*api.CreateWorkgroupResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	ids, err := s.defaultSubnets(ctx, in.SubnetIds)
	if err != nil {
		return nil, err
	}

	id := uuid.NewV4().String()
	w := &api.Workgroup{
		WorkgroupName:      in.WorkgroupName,
		NamespaceName:      in.NamespaceName,
		WorkgroupId:        new(id),
		WorkgroupArn:       new(arn(scope, kindWorkgroup, id)),
		BaseCapacity:       common.OrElse(in.BaseCapacity, new(int64(128))),
		MaxCapacity:        in.MaxCapacity,
		Port:               common.OrElse(in.Port, new(int64(5439))),
		PubliclyAccessible: common.OrElse(in.PubliclyAccessible, new(false)),
		EnhancedVpcRouting: common.OrElse(in.EnhancedVpcRouting, new(false)),
		ConfigParameters:   in.ConfigParameters,
		SubnetIds:          ids,
		SecurityGroupIds:   in.SecurityGroupIds,
		CreationDate:       new(time.Now().UTC()),
		Status:             new("CREATING"),
	}

	if validationErr := validateWorkgroup(w); validationErr != nil {
		return nil, validationErr
	}

	w.Endpoint = &api.Endpoint{
		Address: new(
			fmt.Sprintf(
				"%s.%s.%s.redshift-serverless.amazonaws.com",
				common.Value(w.WorkgroupName),
				scope.AccountID,
				scope.Region,
			),
		),
		Port: w.Port,
	}

	r := &record{
		Name:      common.Value(w.WorkgroupName),
		Kind:      kindWorkgroup,
		ARN:       *w.WorkgroupArn,
		Workgroup: w,
		Tags:      tags,
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, e := read(tx, scope, kindNamespace, common.Value(w.NamespaceName)); e != nil {
			return e
		}

		rows, e := storage.Records[record](tx, scope.Prefix(serviceName, kindWorkgroup))
		if e != nil {
			return e
		}

		for _, old := range rows {
			if common.Value(old.Workgroup.NamespaceName) == common.Value(w.NamespaceName) {
				return conflict("Namespace already has a workgroup")
			}
		}

		_, groups, e := s.networkReferences(tx, scope, w.SubnetIds, w.SecurityGroupIds)
		if e != nil {
			return e
		}

		w.SecurityGroupIds = groups
		if e := insert(tx, scope, r); e != nil {
			return e
		}

		return queue(tx, scope, r)
	})
	return &api.CreateWorkgroupResponse{Workgroup: w}, err
}

func (s *Service) UpdateWorkgroup(
	ctx context.Context,
	in *api.UpdateWorkgroupRequest,
) (*api.UpdateWorkgroupResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	count := 0
	for _, set := range []bool{
		in.BaseCapacity != nil, in.MaxCapacity != nil, in.Port != nil, in.PubliclyAccessible != nil,
		in.EnhancedVpcRouting != nil, in.SubnetIds != nil, in.SecurityGroupIds != nil, in.ConfigParameters != nil,
	} {
		if set {
			count++
		}
	}

	if count != 1 {
		return nil, invalid("UpdateWorkgroup changes exactly one property per request")
	}

	var r *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var e error
		r, e = read(tx, scope, kindWorkgroup, common.Value(in.WorkgroupName))
		if e != nil {
			return e
		}

		w := r.Workgroup
		if common.Value(w.Status) != statusAvailable {
			return conflict("Workgroup must be AVAILABLE")
		}

		if in.BaseCapacity != nil {
			w.BaseCapacity = in.BaseCapacity
		}

		if in.MaxCapacity != nil {
			w.MaxCapacity = in.MaxCapacity
		}

		if in.Port != nil {
			w.Port = in.Port
			w.Endpoint.Port = in.Port
		}

		if in.PubliclyAccessible != nil {
			w.PubliclyAccessible = in.PubliclyAccessible
		}

		if in.EnhancedVpcRouting != nil {
			w.EnhancedVpcRouting = in.EnhancedVpcRouting
		}

		if in.SubnetIds != nil {
			w.SubnetIds = in.SubnetIds
		}

		if in.SecurityGroupIds != nil {
			w.SecurityGroupIds = in.SecurityGroupIds
		}

		if in.ConfigParameters != nil {
			w.ConfigParameters = in.ConfigParameters
		}

		if validationErr := validateWorkgroup(w); validationErr != nil {
			return validationErr
		}

		_, groups, e := s.networkReferences(tx, scope, w.SubnetIds, w.SecurityGroupIds)
		if e != nil {
			return e
		}

		w.SecurityGroupIds = groups
		w.Status = new(statusModifying)
		return queue(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateWorkgroupResponse{Workgroup: r.Workgroup}, nil
}

func (s *Service) DeleteWorkgroup(
	ctx context.Context,
	in *api.DeleteWorkgroupRequest,
) (*api.DeleteWorkgroupResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var e error
		r, e = read(tx, scope, kindWorkgroup, common.Value(in.WorkgroupName))
		if e != nil {
			return e
		}

		rows, e := storage.Records[record](tx, scope.Prefix(serviceName, kindEndpoint))
		if e != nil {
			return e
		}

		for _, ep := range rows {
			if common.Value(ep.Endpoint.WorkgroupName) == r.Name {
				return conflict("Delete endpoint access before the workgroup")
			}
		}

		r.Workgroup.Status = new(statusDeleting)
		tx.Delete(scope.Key(serviceName, r.Kind, r.Name))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteWorkgroupResponse{Workgroup: r.Workgroup}, nil
}

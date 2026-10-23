package ec2

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const kindFlowLog = "flow-log"

type flowLog struct {
	Tags          map[string]string `json:",omitempty"`
	ID            string
	VPCID         string
	ResourceID    string
	ResourceType  string
	ClientToken   string
	Created       time.Time
	Configuration api.FlowLog
}

func flowNotFound(id string) error { return notFound("InvalidFlowLogId.NotFound", "flow log", id) }

func flowValues(f *flowLog) values {
	return values{
		"flow-log-id":    {f.ID},
		filterVPCID:      {f.VPCID},
		filterResourceID: {f.ResourceID},
		"log-group-name": {
			common.Value(f.Configuration.LogGroupName),
		},
		"traffic-type":         {common.Value(f.Configuration.TrafficType)},
		"log-destination-type": {common.Value(f.Configuration.LogDestinationType)},
		"deliver-log-status":   {"SUCCESS"},
		"flow-log-status":      {"ACTIVE"},
	}.withTags(f.Tags)
}

func flowOutput(f *flowLog) api.FlowLog {
	out := *common.Clone(&f.Configuration)
	out.FlowLogId, out.ResourceId = new(f.ID), new(f.ResourceID)
	out.CreationTime = new(f.Created)
	out.FlowLogStatus, out.DeliverLogsStatus, out.Tags = new("ACTIVE"), new("SUCCESS"), tagList(f.Tags)
	return out
}

func flowResource(tx storage.ReadTx, scope request.Scope, kind, id string) (string, error) {
	switch kind {
	case "VPC":
		v, err := loadVPC(tx, scope, id)
		if err != nil {
			return "", err
		}

		return v.ID, nil
	case "Subnet":
		sn, ok, err := storage.Read[subnet](tx, scope.Key("ec2", kindSubnet, id))
		if err != nil || !ok {
			return "", common.ErrorsOr(err, subnetNotFound(id))
		}

		return sn.VPCID, nil
	case "NetworkInterface":
		ni, ok, err := storage.Read[networkInterface](tx, scope.Key("ec2", kindNetworkInterface, id))
		if err != nil {
			return "", err
		}

		if ok {
			return ni.VPCID, nil
		}

		managed, err := managedInterfaces(tx, scope)
		if err != nil {
			return "", err
		}

		for _, ni := range managed {
			if ni.id == id {
				return ni.vpcID, nil
			}
		}

		return "", eniNotFound(id)
	default:
		return "", awserror.New("NotImplemented", "Unsupported flow-log resource type: "+kind)
	}
}

func checkFlowLog(in *api.CreateFlowLogsRequest) (api.FlowLog, error) {
	destination := cmp.Or(common.Value(in.LogDestinationType), "cloud-watch-logs")
	traffic := common.Value(in.TrafficType)
	if !slices.Contains([]string{"ACCEPT", "REJECT", "ALL"}, traffic) {
		return api.FlowLog{}, invalidParameter("Invalid TrafficType")
	}

	if !slices.Contains([]string{"VPC", "Subnet", "NetworkInterface"}, common.Value(in.ResourceType)) {
		return api.FlowLog{}, awserror.New(
			"NotImplemented",
			"Only VPC, Subnet and NetworkInterface flow logs are supported",
		)
	}

	if len(in.ResourceIds) == 0 || len(in.ResourceIds) > 300 {
		return api.FlowLog{}, invalidParameter("ResourceIds must contain 1..300 IDs")
	}

	logDestination, group := common.Value(in.LogDestination), common.Value(in.LogGroupName)
	switch destination {
	case "cloud-watch-logs":
		if common.Value(in.DeliverLogsPermissionArn) == "" {
			return api.FlowLog{}, invalidParameter("DeliverLogsPermissionArn is required")
		}

		if (group == "") == (logDestination == "") {
			return api.FlowLog{}, invalidParameter("Specify LogGroupName or LogDestination")
		}

		if logDestination != "" &&
			(!strings.HasPrefix(logDestination, "arn:") || !strings.Contains(logDestination, ":logs:") ||
				!strings.Contains(logDestination, ":log-group:")) {
			return api.FlowLog{}, invalidParameter("Expected a log group ARN")
		}

		if logDestination != "" {
			_, group, _ = strings.Cut(logDestination, ":log-group:")
			group = strings.TrimSuffix(group, ":*")
		}

	case "s3":
		if !strings.HasPrefix(logDestination, "arn:aws:s3:::") ||
			strings.TrimPrefix(logDestination, "arn:aws:s3:::") == "" ||
			slices.Contains(strings.Split(strings.TrimPrefix(logDestination, "arn:aws:s3:::"), "/"), "AWSLogs") {
			return api.FlowLog{}, invalidParameter("Expected an S3 bucket ARN without an AWSLogs subfolder")
		}

		if group != "" {
			return api.FlowLog{}, invalidParameter("LogGroupName only applies to CloudWatch Logs")
		}

	case "kinesis-data-firehose":
		if !strings.HasPrefix(logDestination, "arn:") || !strings.Contains(logDestination, ":firehose:") ||
			!strings.Contains(logDestination, ":deliverystream/") {
			return api.FlowLog{}, invalidParameter("Expected a Firehose stream ARN")
		}

		if group != "" {
			return api.FlowLog{}, invalidParameter("LogGroupName only applies to CloudWatch Logs")
		}

	default:
		return api.FlowLog{}, invalidParameter("Invalid LogDestinationType")
	}

	interval := int64(600)
	if in.MaxAggregationInterval != nil {
		interval = *in.MaxAggregationInterval
	}

	if interval != 60 && interval != 600 {
		return api.FlowLog{}, invalidParameter("MaxAggregationInterval must be 60 or 600")
	}

	if in.DestinationOptions != nil {
		if destination != "s3" {
			return api.FlowLog{}, invalidParameter("DestinationOptions requires S3")
		}

		format := cmp.Or(common.Value(in.DestinationOptions.FileFormat), "plain-text")
		if format != "plain-text" && format != "parquet" {
			return api.FlowLog{}, invalidParameter("Invalid destination file format")
		}
	}

	format := common.Value(in.LogFormat)
	if in.LogFormat != nil {
		fields := strings.Fields(format)
		if len(fields) == 0 {
			return api.FlowLog{}, invalidParameter("LogFormat requires at least one field")
		}
		for _, field := range fields {
			name, ok := strings.CutPrefix(field, "${")
			if !ok || !strings.HasSuffix(name, "}") {
				return api.FlowLog{}, invalidParameter("Invalid LogFormat field")
			}
			name = strings.TrimSuffix(name, "}")
			if !slices.Contains(flowLogFields, name) {
				return api.FlowLog{}, invalidParameter("Unknown LogFormat field: " + name)
			}
		}
	}
	if in.LogFormat != nil && format == "" {
		return api.FlowLog{}, invalidParameter("LogFormat cannot be empty")
	}

	if format == "" {
		format = "${version} ${account-id} ${interface-id} ${srcaddr} ${dstaddr} ${srcport} ${dstport} " +
			"${protocol} ${packets} ${bytes} ${start} ${end} ${action} ${log-status}"
	}

	out := api.FlowLog{
		DeliverLogsPermissionArn: common.Clone(in.DeliverLogsPermissionArn),
		DeliverCrossAccountRole:  common.Clone(in.DeliverCrossAccountRole), LogGroupName: optional(group),
		TrafficType: new(traffic), LogDestinationType: new(destination), LogDestination: optional(logDestination),
		LogFormat: new(format), MaxAggregationInterval: new(interval),
	}

	if in.DestinationOptions != nil {
		out.DestinationOptions = &api.DestinationOptionsResponse{
			FileFormat:               new(cmp.Or(common.Value(in.DestinationOptions.FileFormat), "plain-text")),
			HiveCompatiblePartitions: new(common.Value(in.DestinationOptions.HiveCompatiblePartitions)),
			PerHourPartition:         new(common.Value(in.DestinationOptions.PerHourPartition)),
		}
	}

	return out, nil
}

func (s *Service) CreateFlowLogs(
	ctx context.Context,
	in *api.CreateFlowLogsRequest,
) (*api.CreateFlowLogsResult, error) {
	config, err := checkFlowLog(in)
	if err != nil {
		return nil, err
	}

	tags, err := specTags(in.TagSpecifications, "vpc-flow-log")
	if err != nil {
		return nil, err
	}

	out := &api.CreateFlowLogsResult{
		ClientToken:  in.ClientToken,
		FlowLogIds:   []string{},
		Unsuccessful: []api.UnsuccessfulItem{},
	}

	err = s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		if common.Value(in.LogDestinationType) == "kinesis-data-firehose" {
			parts := strings.Split(common.Value(in.LogDestination), ":")
			if len(parts) > 4 && parts[4] != scope.AccountID && common.Value(in.DeliverLogsPermissionArn) == "" {
				return invalidParameter("Cross-account Firehose delivery requires DeliverLogsPermissionArn")
			}
		}

		all, recordsErr := storage.Records[flowLog](tx, scope.Key("ec2", kindFlowLog, ""))
		if recordsErr != nil {
			return recordsErr
		}

		for _, resourceID := range in.ResourceIds {
			vpcID, flowResourceErr := flowResource(tx, scope, common.Value(in.ResourceType), resourceID)
			if flowResourceErr != nil {
				var resourceErr *awserror.Error
				if !errors.As(flowResourceErr, &resourceErr) {
					return flowResourceErr
				}
				out.Unsuccessful = append(out.Unsuccessful, api.UnsuccessfulItem{
					ResourceId: new(resourceID),
					Error: &api.UnsuccessfulItemError{
						Code:    new(resourceErr.Code),
						Message: new(resourceErr.Message),
					},
				})
				continue
			}

			existing := slices.IndexFunc(all, func(f *flowLog) bool {
				return in.ClientToken != nil && f.ClientToken == *in.ClientToken && f.ResourceID == resourceID
			})
			if existing >= 0 {
				out.FlowLogIds = append(out.FlowLogIds, all[existing].ID)
				continue
			}

			f := &flowLog{
				ID: newID("fl"), ResourceID: resourceID, ResourceType: common.Value(in.ResourceType), VPCID: vpcID,
				Configuration: config, Created: s.now().UTC(), Tags: tags, ClientToken: common.Value(in.ClientToken),
			}

			if writeErr := storage.Write(tx, scope.Key("ec2", kindFlowLog, f.ID), f); writeErr != nil {
				return writeErr
			}

			out.FlowLogIds = append(out.FlowLogIds, f.ID)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeleteFlowLogs(
	ctx context.Context,
	in *api.DeleteFlowLogsRequest,
) (*api.DeleteFlowLogsResult, error) {
	out := &api.DeleteFlowLogsResult{Unsuccessful: []api.UnsuccessfulItem{}}
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		for _, id := range in.FlowLogIds {
			if _, ok := tx.Get(scope.Key("ec2", kindFlowLog, id)); !ok {
				out.Unsuccessful = append(out.Unsuccessful, api.UnsuccessfulItem{
					ResourceId: new(id),
					Error: &api.UnsuccessfulItemError{
						Code:    new("InvalidFlowLogId.NotFound"),
						Message: new("Flow log not found"),
					},
				})
				continue
			}

			tx.Delete(scope.Key("ec2", kindFlowLog, id))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DescribeFlowLogs(
	ctx context.Context,
	in *api.DescribeFlowLogsRequest,
) (*api.DescribeFlowLogsResult, error) {
	match, err := compileFilters(in.Filter, flowValues(&flowLog{}).names()...)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeFlowLogsResult{FlowLogs: []api.FlowLog{}}
	err = s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		all, recordsErr2 := storage.Records[flowLog](tx, scope.Key("ec2", kindFlowLog, ""))
		if recordsErr2 != nil {
			return recordsErr2
		}

		id := func(f *flowLog) string { return f.ID }
		all, recordsErr2 = selectIDs(all, id, in.FlowLogIds, flowNotFound)
		if recordsErr2 != nil {
			return recordsErr2
		}

		all = slices.DeleteFunc(all, func(f *flowLog) bool { return !match.match(flowValues(f)) })
		limit := len(all) + 1
		if in.MaxResults != nil {
			if *in.MaxResults < 1 {
				return invalidParameter("MaxResults must be positive")
			}

			limit = int(*in.MaxResults)
		}

		all, out.NextToken, recordsErr2 = common.Page(
			all,
			id,
			in.NextToken,
			limit,
			invalidParameter("Invalid NextToken"),
		)
		for _, f := range all {
			out.FlowLogs = append(out.FlowLogs, flowOutput(f))
		}

		return recordsErr2
	})
	if err != nil {
		return nil, fmt.Errorf("describe flow logs: %w", err)
	}

	return out, nil
}

// flowLogFields are the non-tag VPC fields from the AWS flow-log records documentation.
var flowLogFields = strings.Fields(
	"version account-id interface-id srcaddr dstaddr srcport dstport protocol packets bytes start end action log-status " +
		"vpc-id subnet-id instance-id tcp-flags type pkt-srcaddr pkt-dstaddr region az-id sublocation-type sublocation-id " +
		"pkt-src-aws-service pkt-dst-aws-service flow-direction traffic-path ecs-cluster-arn ecs-cluster-name " +
		"ecs-container-instance-arn ecs-container-instance-id ecs-container-id ecs-second-container-id ecs-service-name " +
		"ecs-task-definition-arn ecs-task-arn ecs-task-id reject-reason resource-id encryption-status interface-type " +
		"next-hop-interface-id next-hop-subnet-id next-hop-az-id next-hop-vpc-id next-hop-interface-type",
)

package iam

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/google/uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/iam"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// serviceLinkedRoleNames maps service principals to their role-name suffixes, which each
// service defines rather than following a casing rule.
var serviceLinkedRoleNames = map[string]string{
	"autoscaling.amazonaws.com":                 "AutoScaling",
	"dsql.amazonaws.com":                        "AuroraDsql",
	"ecs.amazonaws.com":                         "ECS",
	"ecs.application-autoscaling.amazonaws.com": "ApplicationAutoScaling_ECSService",
	"eks.amazonaws.com":                         "AmazonEKS",
	"eks-fargate.amazonaws.com":                 "AmazonEKSForFargate",
	"eks-nodegroup.amazonaws.com":               "AmazonEKSNodegroup",
	"elasticache.amazonaws.com":                 "ElastiCache",
	"elasticbeanstalk.amazonaws.com":            "ElasticBeanstalk",
	"elasticloadbalancing.amazonaws.com":        "ElasticLoadBalancing",
	"es.amazonaws.com":                          "AmazonElasticsearchService",
	"kafka.amazonaws.com":                       "Kafka",
	"memorydb.amazonaws.com":                    "MemoryDB",
	"mq.amazonaws.com":                          "AmazonMQ",
	"opensearchservice.amazonaws.com":           "AmazonOpenSearchService",
	"rds.amazonaws.com":                         "RDS",
	"replication.dynamodb.amazonaws.com":        "DynamoDBReplication",
	"spot.amazonaws.com":                        "EC2Spot",
	"spotfleet.amazonaws.com":                   "EC2SpotFleet",
}

func (s *Service) CreateServiceLinkedRole(
	ctx context.Context,
	in *api.CreateServiceLinkedRoleRequest,
) (*api.CreateServiceLinkedRoleResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	principal := common.Value(in.AWSServiceName)
	service, ok := serviceLinkedRoleNames[principal]
	if !ok {
		return nil, invalidInput("Unsupported AWS service principal: %s", principal)
	}

	name := "AWSServiceRoleFor" + service
	if in.CustomSuffix != nil {
		if *in.CustomSuffix == "" {
			return nil, invalidInput("CustomSuffix must not be empty")
		}

		name += "_" + *in.CustomSuffix
	}

	if guardErr := checkName("roleName", new(name), roleName); guardErr != nil {
		return nil, guardErr
	}

	if guardErr := checkRoleSettings(in.Description, nil); guardErr != nil {
		return nil, guardErr
	}

	document, err := json.Marshal(map[string]any{"Version": "2012-10-17", "Statement": []any{map[string]any{
		"Effect": "Allow", "Principal": map[string]string{"Service": principal}, "Action": "sts:AssumeRole",
	}}})
	if err != nil {
		return nil, err
	}

	r := &role{
		Name: name, ID: newID("AROA"), Path: "/aws-service-role/" + principal + "/",
		AssumeRolePolicy: string(document), Description: common.Value(in.Description),
		MaxSessionDuration: defaultSessionTime, CreatedAt: s.now().UnixMilli(), ServicePrincipal: principal,
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rk := key(m.AccountID, kindRole, name)
		if _, exists := tx.Get(rk); exists {
			return invalidInput("Service role name %s has been taken", name)
		}

		return storage.Write(tx, rk, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateServiceLinkedRoleResponse{Role: roleOutput(m.AccountID, r, true)}, nil
}

func (s *Service) DeleteServiceLinkedRole(
	ctx context.Context,
	in *api.DeleteServiceLinkedRoleRequest,
) (*api.DeleteServiceLinkedRoleResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	var id string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, rk, loadErr := loadRole(tx, m.AccountID, in.RoleName)
		if loadErr != nil {
			return loadErr
		}

		if r.ServicePrincipal == "" {
			return invalidInput("Role is not a service-linked role")
		}

		id = "task/aws-service-role/" + r.ServicePrincipal + "/" + r.Name + "/" + uuid.NewString()
		tx.Delete(rk)
		return storage.Write(tx, key(m.AccountID, "service-linked-deletion", id), true)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteServiceLinkedRoleResponse{DeletionTaskId: new(id)}, nil
}

func (s *Service) GetServiceLinkedRoleDeletionStatus(
	ctx context.Context,
	in *api.GetServiceLinkedRoleDeletionStatusRequest,
) (*api.GetServiceLinkedRoleDeletionStatusResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if _, ok := tx.Get(key(m.AccountID, "service-linked-deletion", common.Value(in.DeletionTaskId))); !ok {
			return noSuchEntity("Deletion task does not exist")
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.GetServiceLinkedRoleDeletionStatusResponse{Status: new("SUCCEEDED")}, nil
}

func unmodifiableRole(r *role) error {
	if r.ServicePrincipal != "" || strings.HasPrefix(r.Path, "/aws-service-role/") {
		return awserror.New("UnmodifiableEntityException", "Cannot modify this service-linked role")
	}

	return nil
}

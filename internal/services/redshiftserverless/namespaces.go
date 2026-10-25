package redshiftserverless

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"uuid"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/redshiftserverless"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) namespaceReferences(ctx context.Context, n *api.Namespace) error {
	for _, key := range []*string{n.KmsKeyId, n.AdminPasswordSecretKmsKeyId} {
		if key != nil && *key != "" {
			if _, err := s.caller.Call(ctx, "kms", "DescribeKey", map[string]any{"KeyId": *key}); err != nil {
				return invalid("KMS key must exist: " + *key)
			}
		}
	}

	roles := append([]string{}, n.IamRoles...)
	if n.DefaultIamRoleArn != nil && *n.DefaultIamRoleArn != "" {
		roles = append(roles, *n.DefaultIamRoleArn)
	}

	for _, role := range roles {
		name := role[strings.LastIndex(role, "/")+1:]
		out, err := s.caller.Call(ctx, "iam", "GetRole", map[string]any{"RoleName": name})
		if err != nil {
			return invalid("IAM role must exist: " + role)
		}

		found, _ := out["Role"].(map[string]any)
		if found["Arn"] != role {
			return invalid("IAM role ARN does not match an existing role")
		}
	}

	return nil
}

func (s *Service) managedPassword(ctx context.Context, n *api.Namespace, manage *bool, password *string) error {
	if common.Value(manage) && password != nil {
		return invalid("manageAdminPassword conflicts with adminUserPassword")
	}

	if n.AdminPasswordSecretKmsKeyId != nil && !common.Value(manage) && n.AdminPasswordSecretArn == nil {
		return invalid("adminPasswordSecretKmsKeyId requires manageAdminPassword")
	}

	if password != nil && n.AdminPasswordSecretArn != nil && manage == nil {
		return invalid("Disable manageAdminPassword before setting a password")
	}

	if manage == nil {
		return nil
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	if !*manage {
		if n.AdminPasswordSecretArn != nil {
			if secretErr := s.secrets.DeleteManagedSecret(ctx, scope, *n.AdminPasswordSecretArn); secretErr != nil {
				return secretErr
			}
		}

		n.AdminPasswordSecretArn = nil
		n.AdminPasswordSecretKmsKeyId = nil
		return nil
	}

	value, err := json.Marshal(
		map[string]string{"username": common.Value(n.AdminUsername), "password": common.RandomID(32) + "Aa1!"},
	)
	if err != nil {
		return err
	}

	a := common.Value(n.AdminPasswordSecretArn)
	if a == "" {
		a, err = s.secrets.CreateManagedSecret(
			ctx,
			scope,
			"redshift!namespace-"+common.RandomID(24),
			serviceName,
			"Redshift Serverless managed admin password",
			nil,
		)
		if err != nil {
			return err
		}
	}

	params := map[string]any{"SecretId": a, "SecretString": string(value)}

	if n.AdminPasswordSecretKmsKeyId != nil {
		params["KmsKeyId"] = *n.AdminPasswordSecretKmsKeyId
	}

	if _, err := s.caller.Call(ctx, "secretsmanager", "UpdateSecret", params); err != nil {
		return err
	}

	n.AdminPasswordSecretArn = new(a)

	return nil
}

func (s *Service) CreateNamespace(
	ctx context.Context,
	in *api.CreateNamespaceRequest,
) (*api.CreateNamespaceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	id := uuid.NewV4().String()
	n := &api.Namespace{
		NamespaceName:               in.NamespaceName,
		NamespaceId:                 new(id),
		NamespaceArn:                new(arn(scope, kindNamespace, id)),
		AdminUsername:               common.OrElse(in.AdminUsername, new("admin")),
		DbName:                      common.OrElse(in.DbName, new("dev")),
		KmsKeyId:                    in.KmsKeyId,
		DefaultIamRoleArn:           in.DefaultIamRoleArn,
		IamRoles:                    in.IamRoles,
		LogExports:                  in.LogExports,
		CreationDate:                new(time.Now().UTC()),
		Status:                      new(statusAvailable),
		AdminPasswordSecretKmsKeyId: in.AdminPasswordSecretKmsKeyId,
	}

	if validationErr := s.namespaceReferences(ctx, n); validationErr != nil {
		return nil, validationErr
	}

	// Check uniqueness before creating a managed secret; recheck on insertion.
	if _, lookupErr := s.get(ctx, kindNamespace, in.NamespaceName); lookupErr == nil {
		return nil, conflict("Namespace already exists")
	}

	if validationErr := s.managedPassword(ctx, n, in.ManageAdminPassword, in.AdminUserPassword); validationErr != nil {
		return nil, validationErr
	}

	r := &record{
		Name:      common.Value(in.NamespaceName),
		Kind:      kindNamespace,
		ARN:       *n.NamespaceArn,
		Namespace: n,
		Tags:      tags,
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error { return insert(tx, scope, r) })
	return &api.CreateNamespaceResponse{Namespace: n}, err
}

func (s *Service) UpdateNamespace(
	ctx context.Context,
	in *api.UpdateNamespaceRequest,
) (*api.UpdateNamespaceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	r, err := s.get(ctx, kindNamespace, in.NamespaceName)
	if err != nil {
		return nil, err
	}

	n := r.Namespace
	if in.AdminUsername != nil {
		n.AdminUsername = in.AdminUsername
	}

	if in.KmsKeyId != nil {
		n.KmsKeyId = in.KmsKeyId
	}

	if in.DefaultIamRoleArn != nil {
		n.DefaultIamRoleArn = in.DefaultIamRoleArn
	}

	if in.IamRoles != nil {
		n.IamRoles = in.IamRoles
	}

	if in.LogExports != nil {
		n.LogExports = in.LogExports
	}

	if in.AdminPasswordSecretKmsKeyId != nil {
		n.AdminPasswordSecretKmsKeyId = in.AdminPasswordSecretKmsKeyId
	}

	if validationErr := s.namespaceReferences(ctx, n); validationErr != nil {
		return nil, validationErr
	}

	if validationErr := s.managedPassword(ctx, n, in.ManageAdminPassword, in.AdminUserPassword); validationErr != nil {
		return nil, validationErr
	}

	n.Status = new(statusModifying)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, e := read(tx, scope, r.Kind, r.Name); e != nil {
			return e
		}

		return queue(tx, scope, r)
	})
	return &api.UpdateNamespaceResponse{Namespace: n}, err
}

func (s *Service) DeleteNamespace(
	ctx context.Context,
	in *api.DeleteNamespaceRequest,
) (*api.DeleteNamespaceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in.FinalSnapshotRetentionPeriod != nil && in.FinalSnapshotName == nil {
		return nil, invalid("finalSnapshotRetentionPeriod requires finalSnapshotName")
	}

	var r *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var e error
		r, e = read(tx, scope, kindNamespace, common.Value(in.NamespaceName))
		if e != nil {
			return e
		}

		rows, e := storage.Records[record](tx, scope.Prefix(serviceName, kindWorkgroup))
		if e != nil {
			return e
		}

		for _, w := range rows {
			if common.Value(w.Workgroup.NamespaceName) == r.Name {
				return conflict("Namespace still has a workgroup")
			}
		}

		if in.FinalSnapshotName != nil {
			if _, e := snapshotRecord(
				tx,
				scope,
				r,
				in.FinalSnapshotName,
				in.FinalSnapshotRetentionPeriod,
				nil,
			); e != nil {
				return e
			}
		}

		r.Namespace.Status = new(statusDeleting)
		tx.Delete(scope.Key(serviceName, r.Kind, r.Name))
		return nil
	})
	if err != nil {
		return nil, err
	}

	if r.Namespace.AdminPasswordSecretArn != nil {
		if err := s.secrets.DeleteManagedSecret(ctx, scope, *r.Namespace.AdminPasswordSecretArn); err != nil {
			return nil, err
		}
	}

	return &api.DeleteNamespaceResponse{Namespace: r.Namespace}, nil
}

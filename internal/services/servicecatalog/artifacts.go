package servicecatalog

import (
	"context"
	"maps"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/servicecatalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func artifactPrefix(scope request.Scope, productID string) string {
	return scope.Prefix(serviceName, "artifact") + productID + "/"
}

func createArtifact(tx storage.WriteTx, scope request.Scope, productID string,
	p *api.ProvisioningArtifactProperties,
) (*artifact, error) {
	if p == nil {
		return nil, invalid("ProvisioningArtifactParameters are required for a records-only product")
	}

	for key := range p.Info {
		if key != "LoadTemplateFromURL" && key != "TemplateURL" {
			return nil, invalid("Unsupported provisioning artifact Info key %s", key)
		}
	}

	if len(p.Info) != 1 {
		return nil, invalid("Info must contain exactly one template URL source")
	}

	if p.Info["LoadTemplateFromURL"] == "" && p.Info["TemplateURL"] == "" {
		return nil, invalid("Info must contain LoadTemplateFromURL or TemplateURL")
	}

	id := "pa-" + common.RandomID(13)
	typ := p.Type
	if typ == nil {
		typ = new("CLOUD_FORMATION_TEMPLATE")
	}

	a := &artifact{Info: maps.Clone(p.Info), Detail: api.ProvisioningArtifactDetail{
		Id:          new(id),
		Name:        p.Name,
		Description: p.Description,
		Type:        typ,
		CreatedTime: common.SecondsMilli(time.Now()),
		Active:      new(true),
		Guidance:    new("DEFAULT"),
	}}
	err := storage.Write(tx, artifactPrefix(scope, productID)+id, a)
	return a, err
}

func (s *Service) CreateProvisioningArtifact(ctx context.Context, in *api.CreateProvisioningArtifactInput) (
	*api.CreateProvisioningArtifactOutput, error,
) {
	if err := englishLanguage(in.AcceptLanguage); err != nil {
		return nil, err
	}

	return transact(ctx, s, "CreateProvisioningArtifact", in.IdempotencyToken, in,
		func(tx storage.WriteTx, scope request.Scope) (*api.CreateProvisioningArtifactOutput, error) {
			_, err := read[product](tx, scope, kindProduct, common.Value(in.ProductId))
			if err != nil {
				return nil, err
			}

			a, err := createArtifact(tx, scope, *in.ProductId, in.Parameters)
			if err != nil {
				return nil, err
			}

			return &api.CreateProvisioningArtifactOutput{
				ProvisioningArtifactDetail: new(a.Detail),
				Info:                       artifactInfo(a),
				Status:                     new("AVAILABLE"),
			}, nil
		})
}

func findArtifact(tx storage.ReadTx, scope request.Scope, productID string, id, name *string) (*artifact, error) {
	if (id == nil) == (name == nil) {
		return nil, invalid("Specify exactly one artifact ID or name")
	}

	if id != nil {
		return read[artifact](tx, scope, "artifact", productID+"/"+*id)
	}

	rows, err := storage.Records[artifact](tx, artifactPrefix(scope, productID))
	if err != nil {
		return nil, err
	}

	var found *artifact
	for _, a := range rows {
		if common.Value(a.Detail.Name) == *name {
			if found != nil {
				return nil, invalid("Artifact name is ambiguous")
			}

			found = a
		}
	}

	if found == nil {
		return nil, missing("artifact", *name)
	}

	return found, nil
}

func (s *Service) DescribeProvisioningArtifact(ctx context.Context, in *api.DescribeProvisioningArtifactInput) (
	*api.DescribeProvisioningArtifactOutput, error,
) {
	if err := englishLanguage(in.AcceptLanguage); err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeProvisioningArtifactOutput{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		p, e := findProduct(tx, scope, in.ProductId, in.ProductName)
		if e != nil {
			return e
		}

		a, e := findArtifact(
			tx,
			scope,
			*p.Detail.ProductViewSummary.ProductId,
			in.ProvisioningArtifactId,
			in.ProvisioningArtifactName,
		)
		if e != nil {
			return e
		}

		out.ProvisioningArtifactDetail = new(a.Detail)
		out.Info = artifactInfo(a)
		out.Status = new("AVAILABLE")
		return nil
	})
	return out, err
}

func (s *Service) ListProvisioningArtifacts(ctx context.Context, in *api.ListProvisioningArtifactsInput) (
	*api.ListProvisioningArtifactsOutput, error,
) {
	if err := englishLanguage(in.AcceptLanguage); err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListProvisioningArtifactsOutput{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		_, e := read[product](tx, scope, kindProduct, common.Value(in.ProductId))
		if e != nil {
			return e
		}

		rows, e := storage.Records[artifact](tx, artifactPrefix(scope, *in.ProductId))
		if e != nil {
			return e
		}

		for _, a := range rows {
			out.ProvisioningArtifactDetails = append(out.ProvisioningArtifactDetails, a.Detail)
		}

		return nil
	})
	return out, err
}

func (s *Service) UpdateProvisioningArtifact(ctx context.Context, in *api.UpdateProvisioningArtifactInput) (
	*api.UpdateProvisioningArtifactOutput, error,
) {
	if err := englishLanguage(in.AcceptLanguage); err != nil {
		return nil, err
	}

	return transact(ctx, s, "UpdateProvisioningArtifact", nil, in,
		func(tx storage.WriteTx, scope request.Scope) (*api.UpdateProvisioningArtifactOutput, error) {
			a, err := findArtifact(tx, scope, common.Value(in.ProductId), in.ProvisioningArtifactId, nil)
			if err != nil {
				return nil, err
			}

			if in.Name != nil {
				a.Detail.Name = in.Name
			}

			if in.Description != nil {
				a.Detail.Description = in.Description
			}

			if in.Active != nil {
				a.Detail.Active = in.Active
			}

			if in.Guidance != nil {
				a.Detail.Guidance = in.Guidance
			}

			err = storage.Write(tx, artifactPrefix(scope, *in.ProductId)+*in.ProvisioningArtifactId, a)
			return &api.UpdateProvisioningArtifactOutput{
				ProvisioningArtifactDetail: new(a.Detail),
				Info:                       artifactInfo(a),
				Status:                     new("AVAILABLE"),
			}, err
		})
}

func (s *Service) DeleteProvisioningArtifact(ctx context.Context, in *api.DeleteProvisioningArtifactInput) (
	*api.DeleteProvisioningArtifactOutput, error,
) {
	if err := englishLanguage(in.AcceptLanguage); err != nil {
		return nil, err
	}

	return transact(ctx, s, "DeleteProvisioningArtifact", nil, in,
		func(tx storage.WriteTx, scope request.Scope) (*api.DeleteProvisioningArtifactOutput, error) {
			_, err := findArtifact(tx, scope, common.Value(in.ProductId), in.ProvisioningArtifactId, nil)
			if err != nil {
				return nil, err
			}

			if len(tx.Keys(artifactPrefix(scope, *in.ProductId))) == 1 {
				return nil, invalid("Cannot delete the last provisioning artifact")
			}

			tx.Delete(artifactPrefix(scope, *in.ProductId) + *in.ProvisioningArtifactId)
			return &api.DeleteProvisioningArtifactOutput{}, nil
		})
}

// AWS exposes the stored source URL as TemplateUrl, rather than its creation input key.
func artifactInfo(a *artifact) map[string]string {
	url := a.Info["LoadTemplateFromURL"]
	if url == "" {
		url = a.Info["TemplateURL"]
	}

	return map[string]string{"TemplateUrl": url}
}

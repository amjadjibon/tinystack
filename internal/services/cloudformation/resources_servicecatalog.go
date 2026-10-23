package cloudformation

import (
	"context"
	"encoding/json"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/common"
)

const (
	scPortfolioName      = "PortfolioName"
	scService            = "servicecatalog"
	scPortfolioID        = "PortfolioId"
	scProductID          = "ProductId"
	scArtifactParameters = "ProvisioningArtifactParameters"
	scDisplayName        = "DisplayName"
	scProviderName       = "ProviderName"
	scPrincipalARN       = "PrincipalARN"
	scPrincipalType      = "PrincipalType"
	scLocalRoleName      = "LocalRoleName"
	scRoleArn            = "RoleArn"
)

func scRemove(op string) func(context.Context, *client, *resource) error {
	return func(ctx context.Context, c *client, r *resource) error {
		_, err := c.call(ctx, scService, op, map[string]any{"Id": r.Physical})
		if gone(err, "ResourceNotFoundException") {
			return nil
		}

		return err
	}
}

func scUpdateTags(params map[string]any, r *resource, p props) {
	set, removed := tagChanges(props(r.Properties).tags(), p.tags())
	if len(set) > 0 {
		params["AddTags"] = tagList(set, memberKey, memberValue)
	}

	if len(removed) > 0 {
		params["RemoveTags"] = removed
	}
}

func scParams(p props, fields []string) map[string]any {
	params := map[string]any{}
	for _, field := range fields {
		if p.has(field) {
			params[field] = p[field]
		}
	}

	return params
}

func scArtifacts(p props) ([]any, error) {
	items, ok := p[scArtifactParameters].([]any)
	if !ok || len(items) == 0 {
		return nil, awserror.Validation("ProvisioningArtifactParameters must contain a version")
	}

	for _, raw := range items {
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, awserror.Validation("Provisioning artifact must be an object")
		}

		for key := range m {
			switch key {
			case memberName, memberDescription, "Info", memberType:
			default:
				return nil, awserror.Validation("Unsupported provisioning artifact property %s", key)
			}
		}
	}

	return items, nil
}

func scProductAttributes(out map[string]any, ids, names []any) map[string]any {
	return map[string]any{
		"ProductName":               outText(out, "ProductViewDetail", "ProductViewSummary", memberName),
		"ProvisioningArtifactIds":   ids,
		"ProvisioningArtifactNames": names,
		"Id":                        outText(out, "ProductViewDetail", "ProductViewSummary", scProductID),
	}
}

func init() {
	portfolioFields := []string{scDisplayName, scProviderName, memberDescription}
	register("AWS::ServiceCatalog::Portfolio", &resourceType{
		properties: []string{scDisplayName, scProviderName, memberDescription, memberTags},
		attributes: []string{scPortfolioName, "Id"},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := scParams(p, portfolioFields)
			params["IdempotencyToken"] = common.RandomID(32)
			params[memberTags] = tagList(p.tags(), memberKey, memberValue)
			out, err := c.call(ctx, scService, "CreatePortfolio", params)
			if err != nil {
				return "", nil, err
			}

			return outText(out, "PortfolioDetail", "Id"), map[string]any{
				scPortfolioName: p.str(scDisplayName),
				"Id":            outText(out, "PortfolioDetail", "Id"),
			}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := scParams(p, portfolioFields)
			if !p.has(memberDescription) {
				params[memberDescription] = ""
			}

			params["Id"] = r.Physical
			scUpdateTags(params, r, p)
			_, err := c.call(ctx, scService, "UpdatePortfolio", params)
			return map[string]any{scPortfolioName: p.str(scDisplayName), "Id": r.Physical}, err
		},
		remove: scRemove("DeletePortfolio"),
	})
	productFields := []string{
		memberName,
		"Owner",
		memberDescription,
		"Distributor",
		"SupportDescription",
		"SupportEmail",
		"SupportUrl",
	}

	register("AWS::ServiceCatalog::CloudFormationProduct", &resourceType{
		properties: append(append([]string{}, productFields...), memberTags, scArtifactParameters, "ProductType"),
		attributes: []string{"ProductName", "ProvisioningArtifactIds", "ProvisioningArtifactNames", "Id"},
		replace:    []string{scArtifactParameters, "ProductType"},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			artifacts, err := scArtifacts(p)
			if err != nil {
				return "", nil, err
			}

			params := scParams(p, productFields)
			params["ProductType"] = "CLOUD_FORMATION_TEMPLATE"
			if p.has("ProductType") {
				params["ProductType"] = p["ProductType"]
			}

			params["IdempotencyToken"] = common.RandomID(32)
			params[scArtifactParameters] = artifacts[0]
			params[memberTags] = tagList(p.tags(), memberKey, memberValue)
			out, err := c.call(ctx, scService, "CreateProduct", params)
			if err != nil {
				return "", nil, err
			}

			id := outText(out, "ProductViewDetail", "ProductViewSummary", scProductID)
			ids := []any{outText(out, "ProvisioningArtifactDetail", "Id")}
			names := []any{outText(out, "ProvisioningArtifactDetail", memberName)}
			for _, a := range artifacts[1:] {
				version, e := c.call(ctx, scService, "CreateProvisioningArtifact", map[string]any{
					scProductID:             id,
					propertyBatchParameters: a, "IdempotencyToken": common.RandomID(32),
				})
				if e != nil {
					_, cleanupErr := c.call(ctx, scService, "DeleteProduct", map[string]any{"Id": id})
					if cleanupErr != nil {
						return "", nil, cleanupErr
					}

					return "", nil, e
				}

				ids = append(ids, outText(version, "ProvisioningArtifactDetail", "Id"))
				names = append(names, outText(version, "ProvisioningArtifactDetail", memberName))
			}

			return id, scProductAttributes(out, ids, names), nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := scParams(p, productFields)
			for _, field := range productFields[2:] {
				if !p.has(field) {
					params[field] = ""
				}
			}

			params["Id"] = r.Physical
			scUpdateTags(params, r, p)
			_, err := c.call(ctx, scService, "UpdateProduct", params)
			attrs := map[string]any{}
			for key, value := range r.Attributes {
				attrs[key] = value
			}

			attrs["ProductName"] = p.str(memberName)
			return attrs, err
		},
		remove: scRemove("DeleteProduct"),
	})
	for _, principal := range []bool{false, true} {
		typ := "AWS::ServiceCatalog::PortfolioProductAssociation"
		associate := "AssociateProductWithPortfolio"
		disassociate := "DisassociateProductFromPortfolio"
		fields := []string{scPortfolioID, scProductID}
		if principal {
			typ = "AWS::ServiceCatalog::PortfolioPrincipalAssociation"
			associate = "AssociatePrincipalWithPortfolio"
			disassociate = "DisassociatePrincipalFromPortfolio"
			fields = []string{scPortfolioID, scPrincipalARN, scPrincipalType}
		}

		register(typ, &resourceType{
			properties: fields, replace: fields,
			create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
				_, err := c.call(ctx, scService, associate, scParams(p, fields))
				return common.RandomID(32), nil, err
			},
			remove: func(ctx context.Context, c *client, r *resource) error {
				_, err := c.call(ctx, scService, disassociate, scParams(props(r.Properties), fields))
				if gone(err, "ResourceNotFoundException") {
					return nil
				}

				return err
			},
		})
	}

	register("AWS::ServiceCatalog::LaunchRoleConstraint", &resourceType{
		properties: []string{scPortfolioID, scProductID, memberDescription, scRoleArn, scLocalRoleName},
		replace:    []string{scPortfolioID, scProductID},
		attributes: []string{"Id"},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			parameters, err := json.Marshal(scParams(p, []string{scRoleArn, scLocalRoleName}))
			if err != nil {
				return "", nil, err
			}

			params := scParams(p, []string{scPortfolioID, scProductID, memberDescription})
			params[memberType] = "LAUNCH"
			params[propertyBatchParameters] = string(parameters)
			params["IdempotencyToken"] = common.RandomID(32)
			out, err := c.call(ctx, scService, "CreateConstraint", params)
			if err != nil {
				return "", nil, err
			}

			id := outText(out, "ConstraintDetail", "ConstraintId")
			return id, map[string]any{"Id": id}, err
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			parameters, err := json.Marshal(scParams(p, []string{scRoleArn, scLocalRoleName}))
			if err != nil {
				return nil, err
			}

			_, err = c.call(ctx, scService, "UpdateConstraint", map[string]any{
				"Id":              r.Physical,
				memberDescription: p.str(memberDescription), propertyBatchParameters: string(parameters),
			})
			return r.Attributes, err
		},
		remove: scRemove("DeleteConstraint"),
	})
}

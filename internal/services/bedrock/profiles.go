package bedrock

import (
	"context"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrock"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type inferenceProfile struct {
	Detail  api.GetInferenceProfileResponse
	Request *api.CreateInferenceProfileRequest
	Tags    map[string]string
	Token   string
}

func systemProfiles(scope request.Scope) []api.GetInferenceProfileResponse {
	out := []api.GetInferenceProfileResponse{}
	for i := range models {
		model := &models[i]
		if model.Family != familyAnthropic {
			continue
		}

		id := "us." + model.ID
		out = append(out, api.GetInferenceProfileResponse{
			InferenceProfileId:   new(id),
			InferenceProfileName: new("US " + model.Name),
			InferenceProfileArn: new(
				resourceARN(scope, "inference-profile", id),
			),
			Status: new("ACTIVE"),
			Type:   new("SYSTEM_DEFINED"),
			Models: []api.InferenceProfileModel{
				{ModelArn: summary(model, "us-east-1").ModelArn},
				{ModelArn: summary(model, "us-west-2").ModelArn},
			},
		})
	}

	return out
}

// GetInferenceProfile returns a system or application profile in this account and Region.
func (s *Service) GetInferenceProfile(
	ctx context.Context,
	in *api.GetInferenceProfileRequest,
) (*api.GetInferenceProfileResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	ref := common.Value(in.InferenceProfileIdentifier)
	for _, row := range systemProfiles(scope) {
		if ref == *row.InferenceProfileId || ref == *row.InferenceProfileArn {
			return &row, nil
		}
	}

	id, err := resourceID(scope, applicationProfileKind, ref)
	if err != nil {
		return nil, err
	}

	var row *inferenceProfile
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var ok bool
		var e error
		row, ok, e = storage.Read[inferenceProfile](tx, scope.Key(serviceName, "profile", id))
		if e != nil {
			return e
		}

		if !ok {
			return missing()
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &row.Detail, nil
}

// ListInferenceProfiles lists fixed system profiles and local application profiles.
func (s *Service) ListInferenceProfiles(
	ctx context.Context,
	in *api.ListInferenceProfilesRequest,
) (*api.ListInferenceProfilesResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	n, err := pageSize(in.MaxResults)
	if err != nil {
		return nil, err
	}

	if in.TypeEquals != nil && *in.TypeEquals != "APPLICATION" && *in.TypeEquals != "SYSTEM_DEFINED" {
		return nil, invalid("Invalid profile type.")
	}

	rows := []api.InferenceProfileSummary{}
	appendRow := func(d *api.GetInferenceProfileResponse) {
		if in.TypeEquals != nil && *in.TypeEquals != *d.Type {
			return
		}

		rows = append(rows, api.InferenceProfileSummary{
			InferenceProfileId:   d.InferenceProfileId,
			InferenceProfileArn:  d.InferenceProfileArn,
			InferenceProfileName: d.InferenceProfileName,
			Description:          d.Description,
			Models:               d.Models,
			Status:               d.Status,
			Type:                 d.Type,
			CreatedAt:            d.CreatedAt,
			UpdatedAt:            d.UpdatedAt,
		})
	}

	for _, row := range systemProfiles(scope) {
		appendRow(&row)
	}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, e := storage.Records[inferenceProfile](tx, scope.Prefix(serviceName, "profile"))
		if e != nil {
			return e
		}

		for _, row := range records {
			appendRow(&row.Detail)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		rows,
		func(row api.InferenceProfileSummary) string { return *row.InferenceProfileId },
		in.NextToken,
		n,
		invalid("Invalid nextToken."),
	)
	return &api.ListInferenceProfilesResponse{InferenceProfileSummaries: page, NextToken: next}, err
}

// CreateInferenceProfile copies a foundation model or system profile into an application profile.
func (s *Service) CreateInferenceProfile(
	ctx context.Context,
	in *api.CreateInferenceProfileRequest,
) (*api.CreateInferenceProfileResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if !resourceName.MatchString(common.Value(in.InferenceProfileName)) || in.ModelSource == nil {
		return nil, invalid("A valid name and modelSource are required.")
	}

	source := common.Value(in.ModelSource.CopyFrom)
	if !strings.HasPrefix(source, "arn:aws:bedrock:") {
		return nil, invalid("copyFrom must be a model or system inference profile ARN.")
	}

	model, err := s.Resolve(ctx, source)
	if err != nil {
		return nil, err
	}

	tags, err := checkedTags(in.Tags)
	if err != nil {
		return nil, err
	}

	id := common.RandomID(12, "0123456789abcdefghijklmnopqrstuvwxyz")
	now := time.Now().UTC()
	row := inferenceProfile{Detail: api.GetInferenceProfileResponse{
		InferenceProfileName: in.InferenceProfileName, Description: in.Description,
		InferenceProfileId: new(id), InferenceProfileArn: new(resourceARN(scope, applicationProfileKind, id)),
		Status: new("ACTIVE"), Type: new("APPLICATION"), CreatedAt: new(now), UpdatedAt: new(now),
		Models: []api.InferenceProfileModel{{ModelArn: summary(model, scope.Region).ModelArn}},
	}, Tags: tags, Token: common.Value(in.ClientRequestToken), Request: in}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		records, e := storage.Records[inferenceProfile](tx, scope.Prefix(serviceName, "profile"))
		if e != nil {
			return e
		}

		for _, existing := range records {
			if row.Token != "" && row.Token == existing.Token {
				if !sameJSON(in, existing.Request) {
					return invalid("clientRequestToken was used with different parameters.")
				}

				row = *existing
				return nil
			}
		}

		return storage.Write(tx, scope.Key(serviceName, "profile", id), row)
	})
	return &api.CreateInferenceProfileResponse{
		InferenceProfileArn: row.Detail.InferenceProfileArn,
		Status:              row.Detail.Status,
	}, err
}

// DeleteInferenceProfile deletes an application profile; system profiles are immutable.
func (s *Service) DeleteInferenceProfile(
	ctx context.Context,
	in *api.DeleteInferenceProfileRequest,
) (*api.DeleteInferenceProfileResponse, error) {
	detail, err := s.GetInferenceProfile(
		ctx,
		&api.GetInferenceProfileRequest{InferenceProfileIdentifier: in.InferenceProfileIdentifier},
	)
	if err != nil {
		return nil, err
	}

	if *detail.Type != "APPLICATION" {
		return nil, invalid("System profiles cannot be deleted.")
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Delete(scope.Key(serviceName, "profile", *detail.InferenceProfileId))
		return nil
	})
	return &api.DeleteInferenceProfileResponse{}, err
}

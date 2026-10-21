package elasticbeanstalk

import (
	"context"
	"errors"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/elasticbeanstalk"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func resource(sc request.Scope, r *record) (kind, name, resourceARN string, detail any) {
	switch {
	case r.Application != nil:
		return kindApplication, *r.Application.ApplicationName, *r.Application.ApplicationArn, r.Application
	case r.Version != nil:
		return kindApplicationVersion, *r.Version.VersionLabel, *r.Version.ApplicationVersionArn, r.Version
	case r.Template != nil:
		name = *r.Template.ApplicationName + "/" + *r.Template.TemplateName
		return kindConfigurationTemplate, *r.Template.TemplateName, *arn(sc, kindConfigurationTemplate, name), r.Template
	default:
		return kindEnvironment, *r.Environment.EnvironmentName, *r.Environment.EnvironmentArn, r.Environment
	}
}

func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(k, serviceName)
			if !ok {
				continue
			}

			kind, _, _ := strings.Cut(rest, "/")
			switch kind {
			case kindApplication, kindVersion, kindTemplate, kindEnvironment:
			default:
				continue
			}

			r, _, e := storage.Read[record](tx, k)
			if e != nil {
				return e
			}

			if r.Environment != nil && *r.Environment.Status == terminated {
				continue
			}

			kind, name, a, d := resource(request.Scope{AccountID: account, Region: region}, r)
			out = append(
				out,
				catalog.Resource{
					Account: account,
					Region:  region,
					Type:    kind,
					Name:    name,
					ARN:     a,
					Tags:    common.CloneTags(r.Tags),
					Details: map[string]any{"RecordsOnly": true, "ExecutionReady": false, "Configuration": d},
				},
			)
		}

		return nil
	})

	return out, err
}

func tagged(tx storage.ReadTx, sc request.Scope, a *string) (*record, string, error) {
	if a == nil {
		return nil, "", invalid("ResourceArn required.")
	}

	for _, kind := range []string{kindApplication, kindVersion, kindTemplate, kindEnvironment} {
		for _, k := range tx.Keys(sc.Prefix(serviceName, kind)) {
			r, _, err := storage.Read[record](tx, k)
			if err != nil {
				return nil, "", err
			}

			_, _, value, _ := resource(sc, r)
			if value == *a {
				return r, k, nil
			}
		}
	}

	return nil, "", awserror.New("ResourceNotFoundException", "Resource not found.")
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceMessage,
) (*api.ResourceTagsDescriptionMessage, error) {
	out := &api.ResourceTagsDescriptionMessage{ResourceArn: in.ResourceArn}
	err := s.view(ctx, func(tx storage.ReadTx, sc request.Scope) error {
		r, _, e := tagged(tx, sc, in.ResourceArn)
		if e != nil {
			return e
		}

		out.ResourceTags = tagging.List(
			r.Tags,
			func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} },
		)
		return nil
	})

	return out, err
}

func (s *Service) UpdateTagsForResource(
	ctx context.Context,
	in *api.UpdateTagsForResourceMessage,
) (*api.UpdateTagsForResourceOutput, error) {
	add, err := tags(in.TagsToAdd)
	if err != nil {
		return nil, err
	}

	for _, k := range in.TagsToRemove {
		if _, ok := add[k]; ok {
			return nil, invalid("Cannot add and remove the same tag.")
		}

		if tagging.Reserved(k) {
			return nil, invalid("Cannot remove reserved tags.")
		}
	}

	err = s.change(ctx, func(tx storage.WriteTx, sc request.Scope) error {
		r, k, e := tagged(tx, sc, in.ResourceArn)
		if e != nil {
			return e
		}

		r.Tags, e = tagging.Merge(tagging.Remove(r.Tags, in.TagsToRemove), add)
		if e != nil {
			if errors.Is(e, tagging.ErrLimit) {
				return awserror.New("TooManyTagsException", e.Error())
			}

			return invalid(e.Error())
		}

		return storage.Write(tx, k, r)
	})

	return &api.UpdateTagsForResourceOutput{}, err
}

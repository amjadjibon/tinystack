package elbv2

import (
	"context"
	"maps"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/elbv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const maxTaggedResources = 20

// tagged is a resource's tags and how to save them.
type tagged struct {
	tags map[string]string
	save func(tx storage.WriteTx, tags map[string]string) error
}

// findTagged returns the tags of the load balancer, target group, listener,
// or rule an ARN names.
func findTagged(tx storage.ReadTx, scope request.Scope, arnValue string) (tagged, error) {
	resource := strings.TrimPrefix(arnValue, arn(scope, ""))
	switch {
	case strings.HasPrefix(resource, "loadbalancer/"):
		b, err := findLB(tx, scope, arnValue)
		if err != nil {
			return tagged{}, err
		}

		return tagged{tags: b.Tags, save: func(tx storage.WriteTx, tags map[string]string) error {
			b.Tags = tags
			return storage.Write(tx, key(scope, kindLB, b.name()), b)
		}}, nil
	case strings.HasPrefix(resource, "targetgroup/"):
		t, err := findTG(tx, scope, arnValue)
		if err != nil {
			return tagged{}, err
		}

		return tagged{tags: t.Tags, save: func(tx storage.WriteTx, tags map[string]string) error {
			t.Tags = tags
			return storage.Write(tx, key(scope, kindTG, t.name()), t)
		}}, nil
	case strings.HasPrefix(resource, "listener/"):
		l, err := findListener(tx, scope, arnValue)
		if err != nil {
			return tagged{}, err
		}

		return tagged{tags: l.Tags, save: func(tx storage.WriteTx, tags map[string]string) error {
			l.Tags = tags
			return storage.Write(tx, key(scope, kindListener, l.Balancer+"/"+l.ID), l)
		}}, nil
	case strings.HasPrefix(resource, "listener-rule/"):
		l, r, err := findRule(tx, scope, arnValue)
		switch {
		case err != nil:
			return tagged{}, err
		case r == nil:
			return tagged{tags: l.RuleTags, save: func(tx storage.WriteTx, tags map[string]string) error {
				l.RuleTags = tags
				return storage.Write(tx, key(scope, kindListener, l.Balancer+"/"+l.ID), l)
			}}, nil
		}

		return tagged{tags: r.Tags, save: func(tx storage.WriteTx, tags map[string]string) error {
			r.Tags = tags
			return storage.Write(tx, key(scope, kindRule, r.Listener+"/"+r.ID), r)
		}}, nil
	}

	return tagged{}, validation("'%s' is not a valid Elastic Load Balancing resource ARN", arnValue)
}

// retag changes the tags of each resource in one transaction.
func (s *Service) retag(ctx context.Context, arns []string, change func(map[string]string) error) error {
	m, err := request.Regional(ctx)
	if err != nil {
		return err
	}

	if len(arns) == 0 || len(arns) > maxTaggedResources {
		return validation("ResourceArns must name 1 to 20 resources")
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, a := range arns {
			t, findErr := findTagged(tx, m.Scope, a)
			if findErr != nil {
				return findErr
			}

			tags := maps.Clone(t.tags)
			if tags == nil {
				tags = map[string]string{}
			}

			if changeErr := change(tags); changeErr != nil {
				return changeErr
			}

			if saveErr := t.save(tx, tags); saveErr != nil {
				return saveErr
			}
		}

		return nil
	})
}

func (s *Service) AddTags(ctx context.Context, in *api.AddTagsInput) (*api.AddTagsOutput, error) {
	if len(in.Tags) == 0 {
		return nil, validation("At least one tag must be specified")
	}

	added, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	return &api.AddTagsOutput{}, s.retag(ctx, in.ResourceArns, func(tags map[string]string) error {
		maps.Copy(tags, added)
		return checkTags(tags)
	})
}

func (s *Service) RemoveTags(ctx context.Context, in *api.RemoveTagsInput) (*api.RemoveTagsOutput, error) {
	if len(in.TagKeys) == 0 {
		return nil, validation("At least one tag key must be specified")
	}

	return &api.RemoveTagsOutput{}, s.retag(ctx, in.ResourceArns, func(tags map[string]string) error {
		for _, k := range in.TagKeys {
			delete(tags, k)
		}

		return nil
	})
}

func (s *Service) DescribeTags(ctx context.Context, in *api.DescribeTagsInput) (*api.DescribeTagsOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if len(in.ResourceArns) == 0 || len(in.ResourceArns) > maxTaggedResources {
		return nil, validation("ResourceArns must name 1 to 20 resources")
	}

	out := &api.DescribeTagsOutput{TagDescriptions: []api.TagDescription{}}
	return out, s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, a := range in.ResourceArns {
			t, findErr := findTagged(tx, m.Scope, a)
			if findErr != nil {
				return findErr
			}

			out.TagDescriptions = append(out.TagDescriptions, api.TagDescription{
				ResourceArn: new(a), Tags: tagList(t.tags),
			})
		}

		return nil
	})
}

package eks

import (
	"context"
	"maps"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/eks"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// tagged is a taggable record's tags and how to store the record.
type tagged struct {
	tags *map[string]string
	save func(storage.WriteTx) error
}

// taggedRecord reads the record at key, if its ARN is arn, as tagged.
func taggedRecord[T any](
	tx storage.ReadTx,
	key, resourceARN string,
	fields func(*T) (string, *map[string]string),
) (*tagged, error) {
	v, ok, err := read[T](tx, key)
	if err != nil || !ok {
		return nil, errorsOr(err, resourceNotFound())
	}

	found, tags := fields(v)
	if found != resourceARN {
		return nil, resourceNotFound()
	}

	return &tagged{tags: tags, save: func(tx storage.WriteTx) error { return write(tx, key, v) }}, nil
}

func resourceNotFound() error {
	return awserror.New("NotFoundException", "Resource was not found")
}

// taggable finds the resource an ARN names: a cluster, node group, Fargate
// profile, add-on, access entry, or pod identity association.
func taggable(tx storage.ReadTx, scope request.Scope, resourceARN string) (*tagged, error) {
	rest, ok := strings.CutPrefix(resourceARN, arn(scope, ""))
	if !ok {
		return nil, awserror.New("BadRequestException", "The resource ARN is not an EKS resource of this account "+
			"and Region.")
	}

	parts := strings.Split(rest, "/")
	if len(parts) < 2 {
		return nil, resourceNotFound()
	}

	kind, clusterName := parts[0], parts[1]
	name := ""
	if len(parts) > 2 {
		name = parts[2]
	}

	switch kind {
	case kindCluster:
		return taggedRecord(tx, clusterKey(scope, clusterName), resourceARN,
			func(c *cluster) (string, *map[string]string) { return clusterARN(scope, c.Name), &c.Tags })
	case kindNodegroup:
		return taggedRecord(tx, nodegroupKey(scope, clusterName, name), resourceARN,
			func(n *nodegroup) (string, *map[string]string) { return *n.Group.NodegroupArn, &n.Group.Tags })
	case "fargateprofile":
		return taggedRecord(tx, fargateKey(scope, clusterName, name), resourceARN,
			func(p *fargateProfile) (string, *map[string]string) {
				return *p.Profile.FargateProfileArn, &p.Profile.Tags
			})
	case kindAddon:
		return taggedRecord(tx, addonKey(scope, clusterName, name), resourceARN,
			func(a *addon) (string, *map[string]string) { return *a.Addon.AddonArn, &a.Addon.Tags })
	case "podidentityassociation":
		return taggedRecord(tx, associationKey(scope, clusterName, name), resourceARN,
			func(a *association) (string, *map[string]string) {
				return *a.Association.AssociationArn, &a.Association.Tags
			})
	case "access-entry":
		entries, err := records[accessEntry](tx, prefix(scope, "access")+clusterName+"/")
		if err != nil {
			return nil, err
		}

		for _, e := range entries {
			if *e.Entry.AccessEntryArn == resourceARN {
				return taggedRecord(tx, entryKey(scope, clusterName, *e.Entry.PrincipalArn), resourceARN,
					func(e *accessEntry) (string, *map[string]string) { return *e.Entry.AccessEntryArn, &e.Entry.Tags })
			}
		}
	}

	return nil, resourceNotFound()
}

// retag runs fn on the tags of the resource arn names and stores it.
func (s *Service) retag(
	ctx context.Context,
	resourceARN *string,
	fn func(map[string]string) (map[string]string, error),
) error {
	scope, err := identity(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, findErr := taggable(tx, scope, value(resourceARN))
		if findErr != nil {
			return findErr
		}

		tags, fnErr := fn(cloneTags(*r.tags))
		if fnErr != nil {
			return fnErr
		}

		*r.tags = tags
		return r.save(tx)
	})
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	err := s.retag(ctx, in.ResourceArn, func(tags map[string]string) (map[string]string, error) {
		maps.Copy(tags, in.Tags)
		if problem := tagProblem(tags); problem != "" {
			return nil, awserror.New("BadRequestException", problem)
		}

		return tags, nil
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceResponse{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	err := s.retag(ctx, in.ResourceArn, func(tags map[string]string) (map[string]string, error) {
		return tagging.Remove(tags, in.TagKeys), nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceResponse{}, nil
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListTagsForResourceResponse{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, findErr := taggable(tx, scope, value(in.ResourceArn))
		if findErr != nil {
			return findErr
		}

		out.Tags = cloneTags(*r.tags)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

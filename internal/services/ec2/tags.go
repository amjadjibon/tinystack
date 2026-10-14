package ec2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// taggableKind is a resource type that takes tags: its record kind, the
// resource type EC2 reports, and its error for a missing ID.
type taggableKind struct {
	missing      func(id string) error
	kind         string
	resourceType string
}

// taggable maps each resource ID prefix to its resource type. Every record
// of these kinds keeps its tags in a Tags field.
var taggable = map[string]taggableKind{
	"dopt":     {kind: kindDHCPOptions, resourceType: kindDHCPOptions, missing: dhcpNotFound},
	"eigw":     {kind: kindEgressGateway, resourceType: kindEgressGateway, missing: egressNotFound},
	"eni":      {kind: kindNetworkInterface, resourceType: kindNetworkInterface, missing: eniNotFound},
	"fl":       {kind: kindFlowLog, resourceType: "vpc-flow-log", missing: flowNotFound},
	"pl":       {kind: kindPrefixList, resourceType: kindPrefixList, missing: prefixNotFound},
	"pcx":      {kind: kindPeering, resourceType: kindPeering, missing: peeringNotFound},
	"vpc":      {kind: kindVPC, resourceType: "vpc", missing: vpcNotFound},
	"subnet":   {kind: kindSubnet, resourceType: "subnet", missing: subnetNotFound},
	"sg":       {kind: kindSecurityGroup, resourceType: "security-group", missing: groupNotFound},
	ec2Key:     {kind: kindKeyPair, resourceType: "key-pair", missing: keyPairIDNotFound},
	"i":        {kind: kindInstance, resourceType: kindInstance, missing: instanceNotFound},
	"lt":       {kind: kindLaunchTemplate, resourceType: kindLaunchTemplate, missing: templateIDNotFound},
	"igw":      {kind: kindInternetGateway, resourceType: kindInternetGateway, missing: gatewayNotFound},
	"rtb":      {kind: kindRouteTable, resourceType: kindRouteTable, missing: routeTableNotFound},
	"acl":      {kind: kindNetworkACL, resourceType: kindNetworkACL, missing: aclNotFound},
	"eipalloc": {kind: kindAddress, resourceType: "elastic-ip", missing: addressNotFound},
	"nat":      {kind: kindNATGateway, resourceType: "natgateway", missing: natGatewayNotFound},
	"vpce":     {kind: kindVPCEndpoint, resourceType: kindVPCEndpoint, missing: endpointNotFound},
}

// taggableByID returns the resource type of an ID.
func taggableByID(id string) (taggableKind, error) {
	prefix, _, _ := strings.Cut(id, "-")
	t, ok := taggable[prefix]
	if !ok {
		return t, awserror.New("InvalidID", fmt.Sprintf("The ID '%s' is not valid", id))
	}

	return t, nil
}

// tagMap validates request tags: keys are 1 to 128 characters and not
// reserved, and values at most 256.
func tagMap(tags []api.Tag) (map[string]string, error) {
	out := make(map[string]string, len(tags))
	for _, t := range tags {
		k := common.Value(t.Key)
		if k == "" {
			return nil, invalidParameter("Tag keys must not be empty")
		}

		if tagging.Reserved(k) {
			return nil, invalidParameter("Tag keys starting with 'aws:' are reserved for internal use")
		}

		out[k] = common.Value(t.Value)
	}

	return out, checkTags(out)
}

// checkTags reports a tag set that breaks the tagging limits with EC2's
// codes.
func checkTags(tags map[string]string) error {
	switch err := tagging.Check(tags); {
	case errors.Is(err, tagging.ErrLimit):
		return awserror.New("TagLimitExceeded", "The maximum number of Tags for a resource has been reached.")
	case err != nil:
		return invalidParameter(err.Error())
	}

	return nil
}

// specTags returns the tags a create request's TagSpecifications give the
// new resource of resourceType.
func specTags(specs []api.TagSpecification, resourceType string) (map[string]string, error) {
	tags := map[string]string{}
	for _, spec := range specs {
		kind := common.Value(spec.ResourceType)
		if kind != resourceType {
			return nil, invalidParameter(fmt.Sprintf(
				"'%s' is not a valid taggable resource type for this operation.", kind))
		}

		given, err := tagMap(spec.Tags)
		if err != nil {
			return nil, err
		}

		maps.Copy(tags, given)
	}

	return tags, checkTags(tags)
}

func tagList(tags map[string]string) []api.Tag {
	if len(tags) == 0 {
		return nil
	}

	return tagging.List(tags, func(k, v string) api.Tag { return api.Tag{Key: &k, Value: &v} })
}

// retag changes the tags of the resource id names, keeping its record's
// other fields as they are.
func retag(tx storage.WriteTx, scope request.Scope, id string, fn func(map[string]string) error) error {
	t, err := taggableByID(id)
	if err != nil {
		return err
	}

	k := scope.Key("ec2", t.kind, id)
	data, ok := tx.Get(k)
	if !ok {
		return t.missing(id)
	}

	var record map[string]json.RawMessage
	if err = json.Unmarshal(data, &record); err != nil {
		return fmt.Errorf("decode %s: %w", k, err)
	}

	tags := map[string]string{}
	if raw, found := record["Tags"]; found {
		if err = json.Unmarshal(raw, &tags); err != nil {
			return fmt.Errorf("decode %s tags: %w", k, err)
		}
	}

	if fnErr := fn(tags); fnErr != nil {
		return fnErr
	}

	if record["Tags"], err = json.Marshal(tags); err != nil {
		return err
	}

	return storage.Write(tx, k, record)
}

func (s *Service) CreateTags(ctx context.Context, in *api.CreateTagsRequest) (*api.CreateTagsOutput, error) {
	add, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		for _, id := range in.Resources {
			if tagErr := retag(tx, scope, id, func(tags map[string]string) error {
				maps.Copy(tags, add)
				return checkTags(tags)
			}); tagErr != nil {
				return tagErr
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateTagsOutput{}, nil
}

// DeleteTags removes the given tags (a tag with a value only while it has
// that value), or every tag when none are given.
func (s *Service) DeleteTags(ctx context.Context, in *api.DeleteTagsRequest) (*api.DeleteTagsOutput, error) {
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		for _, id := range in.Resources {
			if tagErr := retag(tx, scope, id, func(tags map[string]string) error {
				if len(in.Tags) == 0 {
					clear(tags)
				}

				for _, t := range in.Tags {
					if have, ok := tags[common.Value(t.Key)]; ok && (t.Value == nil || *t.Value == have) {
						delete(tags, common.Value(t.Key))
					}
				}

				return nil
			}); tagErr != nil {
				return tagErr
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteTagsOutput{}, nil
}

// DescribeTags lists the tags of every taggable resource, by resource ID and
// key.
func (s *Service) DescribeTags(ctx context.Context, in *api.DescribeTagsRequest) (*api.DescribeTagsResult, error) {
	match, err := compileFilters(in.Filters, ec2Key, "value", "resource-id", "resource-type")
	if err != nil {
		return nil, err
	}

	var found []*api.TagDescription
	err = s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		for _, prefix := range slices.Sorted(maps.Keys(taggable)) {
			t := taggable[prefix]
			records, listErr := list[struct {
				Tags map[string]string
				ID   string
			}](tx, scope, t.kind)
			if listErr != nil {
				return listErr
			}

			for _, r := range records {
				for _, k := range slices.Sorted(maps.Keys(r.Tags)) {
					if match.match(values{
						ec2Key: {k}, "value": {r.Tags[k]}, "resource-id": {r.ID}, "resource-type": {t.resourceType},
					}) {
						found = append(found, &api.TagDescription{
							ResourceId: new(
								r.ID,
							),
							ResourceType: new(t.resourceType),
							Key:          new(k),
							Value:        new(r.Tags[k]),
						})
					}
				}
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	id := func(d *api.TagDescription) string { return *d.ResourceId + "\x00" + *d.Key }
	slices.SortFunc(found, func(a, b *api.TagDescription) int { return strings.Compare(id(a), id(b)) })
	selected, next, err := page(found, id, in.NextToken, in.MaxResults, maxDescribe)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeTagsResult{Tags: []api.TagDescription{}, NextToken: next}
	for _, d := range selected {
		out.Tags = append(out.Tags, *d)
	}

	return out, nil
}

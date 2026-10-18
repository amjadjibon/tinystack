package eventbridge

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/eventbridge"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// AuthorizationResources attaches the owning bus's policy to bus and rule actions.
// Operations without a bus/rule resource retain wildcard identity evaluation.
func (s *Service) AuthorizationResources(
	ctx context.Context,
	operation string,
	input any,
) ([]catalog.AuthorizationResource, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if in, ok := input.(*api.PutEventsRequest); ok {
		var targets []catalog.AuthorizationResource
		for _, entry := range in.Entries {
			target, resolveErr := s.busAuthorization(ctx, &m, entry.EventBusName, "")
			if resolveErr != nil {
				return nil, resolveErr
			}

			target.Values = map[string][]string{
				"events:source":             {common.Value(entry.Source)},
				"events:detail-type":        {common.Value(entry.DetailType)},
				"events:eventbusinvocation": {"false"},
			}

			targets = append(targets, target)
		}

		return targets, nil
	}

	var ref, ruleName string
	switch operation {
	case "DescribeEventBus", "UpdateEventBus", "DeleteEventBus":
		ref = catalog.InputString(input, "Name")
	case "PutPermission", "RemovePermission", "ListRules":
		ref = catalog.InputString(input, "EventBusName")
	case "PutRule", "DescribeRule", "DeleteRule", "EnableRule", "DisableRule":
		ref, ruleName = catalog.InputString(input, "EventBusName"), catalog.InputString(input, "Name")
	case "PutTargets", "RemoveTargets", "ListTargetsByRule":
		ref, ruleName = catalog.InputString(input, "EventBusName"), catalog.InputString(input, "Rule")
	case "TagResource", "UntagResource", "ListTagsForResource":
		arn := catalog.InputString(input, "ResourceARN")
		request.ReferenceScope(&m, "events", arn)
		if name, ok := strings.CutPrefix(arn, busARN(m.Scope, "")); ok {
			ref = name
		} else if rest, isRule := strings.CutPrefix(arn, ruleARN(m.Scope, defaultBus, "")); isRule {
			var onBus bool
			ref, ruleName, onBus = strings.Cut(rest, "/")
			if !onBus {
				ref, ruleName = defaultBus, rest
			}

		} else {
			return nil, invalid("ResourceARN must name an event bus or rule in the request region.")
		}

	case "CreateEventBus":
		return []catalog.AuthorizationResource{{
			ARN: busARN(m.Scope, catalog.InputString(input, "Name")), TagsKnown: true,
		}}, nil
	default:
		return nil, nil
	}

	target, err := s.busAuthorization(ctx, &m, &ref, ruleName)
	if err != nil {
		return nil, err
	}

	if target.Values == nil {
		target.Values = map[string][]string{}
	}

	switch operation {
	case "PutRule":
		values, unknown, patternErr := rulePatternValues(catalog.InputString(input, "EventPattern"))
		if patternErr != nil {
			return nil, patternErr
		}

		for key, value := range values {
			target.Values[key] = value
		}

		target.UnknownKeys = unknown

	case "PutTargets":
		target.Values["events:targetarn"] = catalog.InputPathStrings(input, "Targets.Arn")
	}

	return []catalog.AuthorizationResource{target}, nil
}

func (s *Service) busAuthorization(
	ctx context.Context,
	metadata *request.Metadata,
	ref *string,
	ruleName string,
) (catalog.AuthorizationResource, error) {
	m := *metadata
	name, err := resolveBus(&m, ref)
	if err != nil {
		return catalog.AuthorizationResource{}, err
	}

	target := catalog.AuthorizationResource{
		ARN: busARN(m.Scope, name), OwnerAccount: m.AccountID, TagsKnown: true,
	}

	if ruleName != "" {
		if !targetID.MatchString(ruleName) {
			return catalog.AuthorizationResource{}, invalid("Invalid rule name.")
		}

		target.ARN = ruleARN(m.Scope, name, ruleName)
	}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		target.Policy = policy(tx, m.Scope, name)
		if ruleName != "" {
			r, _, readErr := storage.Read[rule](tx, rulePrefix(m.Scope, name)+ruleName)
			creator := metadata.AccountID
			if r != nil {
				target.Tags = common.CloneTags(r.Tags)
				creator = r.CreatorAccount
			}

			target.Values = map[string][]string{"events:creatoraccount": {creator}}

			return readErr
		}

		b, _, readErr := loadBus(tx, m.Scope, name)
		if b != nil {
			target.Tags = common.CloneTags(b.Tags)
		}

		return readErr
	})
	return target, err
}

// rulePatternValues supplies literal pattern fields to IAM conditions. Matching
// operators and top-level $or have no literal IAM value, so those fields are
// unknown: a policy condition on them fails closed, and other policies apply.
func rulePatternValues(text string) (values map[string][]string, unknown []string, err error) {
	values = map[string][]string{}
	if text == "" {
		return values, nil, nil
	}

	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(text), &fields) != nil {
		return nil, nil, invalid("Invalid EventPattern.")
	}

	_, hasOr := fields["$or"]
	for _, name := range []string{"source", "detail-type"} {
		key := "events:" + name
		raw, present := fields[name]
		if hasOr {
			unknown = append(unknown, key)
			continue
		}

		if !present {
			continue
		}

		var entries []any
		if json.Unmarshal(raw, &entries) != nil {
			unknown = append(unknown, key)
			continue
		}

		for _, entry := range entries {
			value, literal := entry.(string)
			if !literal {
				delete(values, key)
				unknown = append(unknown, key)
				break
			}

			values[key] = append(values[key], value)
		}
	}

	return values, unknown, nil
}

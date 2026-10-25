package iot

import (
	"context"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/iot"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// maxTargetPolicies is how many policies one target can have attached.
const maxTargetPolicies = 10

// cognitoIdentity is the form of a Cognito identity ID, which can be a
// principal and a policy target.
var cognitoIdentity = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-\d:[0-9a-f-]{36}$`)

// principal records what a certificate, Cognito identity, or thing group is
// attached to: things (for certificates and identities), and policies.
// Exclusive marks a principal attached with EXCLUSIVE_THING.
type principal struct {
	Things    []string
	Policies  []string
	Exclusive bool `json:",omitempty"`
}

func principalKey(scope request.Scope, id string) string {
	return scope.Key(service, kindPrincipal, url.PathEscape(id))
}

// loadPrincipal reads what id is attached to, which is nothing when it has
// no record.
func loadPrincipal(tx storage.ReadTx, scope request.Scope, id string) (*principal, error) {
	p, found, err := storage.Read[principal](tx, principalKey(scope, id))
	if err != nil || !found {
		return &principal{}, err
	}

	return p, nil
}

// save writes what id is attached to, deleting the record once it is
// attached to nothing.
func (p *principal) save(tx storage.WriteTx, scope request.Scope, id string) error {
	if len(p.Things) == 0 && len(p.Policies) == 0 {
		tx.Delete(principalKey(scope, id))
		return nil
	}

	return storage.Write(tx, principalKey(scope, id), p)
}

// detachAll detaches every policy from id, which is being deleted.
func detachAll(tx storage.WriteTx, scope request.Scope, id string) error {
	p, err := loadPrincipal(tx, scope, id)
	if err != nil {
		return err
	}

	for _, name := range p.Policies {
		pol, err := loadPolicy(tx, scope, name)
		if err != nil {
			return err
		}

		pol.Targets = detach(pol.Targets, id)
		if err := pol.save(tx, scope); err != nil {
			return err
		}
	}

	tx.Delete(principalKey(scope, id))
	return nil
}

// checkPrincipal checks that id is a certificate of the caller's account and
// region or a Cognito identity ID.
func checkPrincipal(tx storage.ReadTx, scope request.Scope, id string) error {
	if cert, ok := strings.CutPrefix(id, arn(scope, "cert", "")); ok {
		_, err := loadCertificate(tx, scope, cert)
		return err
	}

	if cognitoIdentity.MatchString(id) {
		return nil
	}

	return invalid("The principal %s is not a certificate ARN of this account and Region or a Cognito identity ID.",
		id)
}

// checkTarget checks a policy target: a principal or a thing group's ARN.
func checkTarget(tx storage.ReadTx, scope request.Scope, id string) error {
	if group, ok := strings.CutPrefix(id, arn(scope, "thinggroup", "")); ok {
		_, err := loadThingGroup(tx, scope, group)
		return err
	}

	return checkPrincipal(tx, scope, id)
}

func (s *Service) AttachPolicy(ctx context.Context, in *api.AttachPolicyRequest) (*api.AttachPolicyOutput, error) {
	target := common.Value(in.Target)
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if err := checkTarget(tx, scope, target); err != nil {
			return err
		}

		pol, err := loadPolicy(tx, scope, common.Value(in.PolicyName))
		if err != nil {
			return err
		}

		p, err := loadPrincipal(tx, scope, target)
		if err != nil {
			return err
		}

		if !slices.Contains(p.Policies, pol.Name) && len(p.Policies) >= maxTargetPolicies {
			return awserror.New("LimitExceededException",
				target+" already has the most policies a target can have attached.")
		}

		p.Policies = attach(p.Policies, pol.Name)
		pol.Targets = attach(pol.Targets, target)
		if err := pol.save(tx, scope); err != nil {
			return err
		}

		return p.save(tx, scope, target)
	})
	if err != nil {
		return nil, err
	}

	return &api.AttachPolicyOutput{}, nil
}

func (s *Service) DetachPolicy(ctx context.Context, in *api.DetachPolicyRequest) (*api.DetachPolicyOutput, error) {
	target := common.Value(in.Target)
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		pol, err := loadPolicy(tx, scope, common.Value(in.PolicyName))
		if err != nil {
			return err
		}

		p, err := loadPrincipal(tx, scope, target)
		if err != nil {
			return err
		}

		p.Policies = detach(p.Policies, pol.Name)
		pol.Targets = detach(pol.Targets, target)
		if err := pol.save(tx, scope); err != nil {
			return err
		}

		return p.save(tx, scope, target)
	})
	if err != nil {
		return nil, err
	}

	return &api.DetachPolicyOutput{}, nil
}

// ListAttachedPolicies lists a target's policies. With recursive, a thing
// group's include its ancestors', and a principal's include those of the
// groups (and their ancestors) of the things it is attached to.
func (s *Service) ListAttachedPolicies(
	ctx context.Context,
	in *api.ListAttachedPoliciesRequest,
) (*api.ListAttachedPoliciesResponse, error) {
	target := common.Value(in.Target)
	var out []api.Policy
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		if err := checkTarget(tx, scope, target); err != nil {
			return err
		}

		names, err := attachedPolicies(tx, scope, target, common.Value(in.Recursive))
		if err != nil {
			return err
		}

		for _, name := range names {
			out = append(out, api.Policy{PolicyName: new(name), PolicyArn: new(arn(scope, kindPolicy, name))})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(out, func(p api.Policy) string { return *p.PolicyName }, in.Marker, in.PageSize)
	if err != nil {
		return nil, err
	}

	return &api.ListAttachedPoliciesResponse{Policies: selected, NextMarker: next}, nil
}

// attachedPolicies returns the sorted names of a target's policies, with
// recursive adding those it inherits through thing groups.
func attachedPolicies(tx storage.ReadTx, scope request.Scope, target string, recursive bool) ([]string, error) {
	p, err := loadPrincipal(tx, scope, target)
	if err != nil || !recursive {
		return p.Policies, err
	}

	var groups []string
	if group, ok := strings.CutPrefix(target, arn(scope, "thinggroup", "")); ok {
		groups = []string{group}
	}

	for _, name := range p.Things {
		t, err := loadThing(tx, scope, name)
		if err != nil {
			return nil, err
		}

		groups = append(groups, t.Groups...)
	}

	names := slices.Clone(p.Policies)
	seen := map[string]bool{}
	for _, name := range groups {
		g, err := loadThingGroup(tx, scope, name)
		if err != nil {
			return nil, err
		}

		above, err := ancestors(tx, scope, g)
		if err != nil {
			return nil, err
		}

		for _, a := range append(above, g) {
			id := arn(scope, "thinggroup", a.Name)
			if seen[id] {
				continue
			}

			seen[id] = true
			gp, err := loadPrincipal(tx, scope, id)
			if err != nil {
				return nil, err
			}

			names = append(names, gp.Policies...)
		}
	}

	slices.Sort(names)
	return slices.Compact(names), nil
}

func (s *Service) ListTargetsForPolicy(
	ctx context.Context,
	in *api.ListTargetsForPolicyRequest,
) (*api.ListTargetsForPolicyResponse, error) {
	var targets []string
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		p, err := loadPolicy(tx, scope, common.Value(in.PolicyName))
		if err != nil {
			return err
		}

		targets = p.Targets
		return nil
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(targets, func(t string) string { return t }, in.Marker, in.PageSize)
	if err != nil {
		return nil, err
	}

	return &api.ListTargetsForPolicyResponse{Targets: selected, NextMarker: next}, nil
}

func (s *Service) AttachThingPrincipal(
	ctx context.Context,
	in *api.AttachThingPrincipalRequest,
) (*api.AttachThingPrincipalResponse, error) {
	kind := common.Value(in.ThingPrincipalType)
	if in.ThingPrincipalType != nil && kind != exclusiveThing && kind != nonExclusive {
		return nil, invalid("The thing principal type must be EXCLUSIVE_THING or NON_EXCLUSIVE_THING.")
	}

	id := common.Value(in.Principal)
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if err := checkPrincipal(tx, scope, id); err != nil {
			return err
		}

		t, err := loadThing(tx, scope, common.Value(in.ThingName))
		if err != nil {
			return err
		}

		p, err := loadPrincipal(tx, scope, id)
		if err != nil {
			return err
		}

		others := slices.DeleteFunc(slices.Clone(p.Things), func(n string) bool { return n == t.Name })
		if len(others) > 0 && (p.Exclusive || kind == exclusiveThing) {
			return invalid("Principal %s is attached to thing %s, and one of the attachments is exclusive.", id,
				others[0])
		}

		p.Things, p.Exclusive = attach(p.Things, t.Name), kind == exclusiveThing
		t.Principals = attach(t.Principals, id)
		if err := storage.Write(tx, scope.Key(service, kindThing, t.Name), t); err != nil {
			return err
		}

		return p.save(tx, scope, id)
	})
	if err != nil {
		return nil, err
	}

	return &api.AttachThingPrincipalResponse{}, nil
}

func (s *Service) DetachThingPrincipal(
	ctx context.Context,
	in *api.DetachThingPrincipalRequest,
) (*api.DetachThingPrincipalResponse, error) {
	id := common.Value(in.Principal)
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		t, err := loadThing(tx, scope, common.Value(in.ThingName))
		if err != nil {
			return err
		}

		p, err := loadPrincipal(tx, scope, id)
		if err != nil {
			return err
		}

		p.Things = detach(p.Things, t.Name)
		p.Exclusive = p.Exclusive && len(p.Things) > 0
		t.Principals = detach(t.Principals, id)
		if err := storage.Write(tx, scope.Key(service, kindThing, t.Name), t); err != nil {
			return err
		}

		return p.save(tx, scope, id)
	})
	if err != nil {
		return nil, err
	}

	return &api.DetachThingPrincipalResponse{}, nil
}

func (s *Service) ListThingPrincipals(
	ctx context.Context,
	in *api.ListThingPrincipalsRequest,
) (*api.ListThingPrincipalsResponse, error) {
	var principals []string
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		t, err := loadThing(tx, scope, common.Value(in.ThingName))
		if err != nil {
			return err
		}

		principals = t.Principals
		return nil
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(principals, func(p string) string { return p }, in.NextToken, in.MaxResults)
	if err != nil {
		return nil, err
	}

	return &api.ListThingPrincipalsResponse{Principals: selected, NextToken: next}, nil
}

func (s *Service) ListPrincipalThings(
	ctx context.Context,
	in *api.ListPrincipalThingsRequest,
) (*api.ListPrincipalThingsResponse, error) {
	id := common.Value(in.Principal)
	var things []string
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		if err := checkPrincipal(tx, scope, id); err != nil {
			return err
		}

		p, err := loadPrincipal(tx, scope, id)
		things = p.Things
		return err
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(things, func(t string) string { return t }, in.NextToken, in.MaxResults)
	if err != nil {
		return nil, err
	}

	return &api.ListPrincipalThingsResponse{Things: selected, NextToken: next}, nil
}

// ListThingPrincipalsV2 lists a thing's principals with their attachment types, optionally only those of one type.
func (s *Service) ListThingPrincipalsV2(
	ctx context.Context, in *api.ListThingPrincipalsV2Request,
) (*api.ListThingPrincipalsV2Response, error) {
	var objects []api.ThingPrincipalObject
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		t, err := loadThing(tx, scope, common.Value(in.ThingName))
		if err != nil {
			return err
		}

		for _, id := range t.Principals {
			p, err := loadPrincipal(tx, scope, id)
			if err != nil {
				return err
			}

			kind := nonExclusive
			if p.Exclusive {
				kind = exclusiveThing
			}

			if in.ThingPrincipalType == nil || *in.ThingPrincipalType == kind {
				objects = append(objects, api.ThingPrincipalObject{Principal: new(id), ThingPrincipalType: new(kind)})
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(
		objects,
		func(p api.ThingPrincipalObject) string { return *p.Principal },
		in.NextToken,
		in.MaxResults,
	)
	if err != nil {
		return nil, err
	}

	return &api.ListThingPrincipalsV2Response{ThingPrincipalObjects: selected, NextToken: next}, nil
}

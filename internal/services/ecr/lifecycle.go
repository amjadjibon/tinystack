package ecr

import (
	"bytes"
	"context"
	"encoding/json"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ecr"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	countMoreThan    = "imageCountMoreThan"
	sincePushed      = "sinceImagePushed"
	statusTagged     = "tagged"
	statusUntagged   = "untagged"
	statusAny        = "any"
	day              = 24 * time.Hour
	lifecycleMessage = "Invalid parameter at 'LifecyclePolicyText' failed to satisfy constraint: "
)

type selection struct {
	CountNumber    *int64   `json:"countNumber"`
	TagStatus      string   `json:"tagStatus"`
	CountType      string   `json:"countType"`
	CountUnit      string   `json:"countUnit,omitempty"`
	TagPrefixList  []string `json:"tagPrefixList,omitempty"`
	TagPatternList []string `json:"tagPatternList,omitempty"`
}

type lifecycleRule struct {
	RulePriority *int   `json:"rulePriority"`
	Description  string `json:"description,omitempty"`
	Action       struct {
		Type string `json:"type"`
	} `json:"action"`
	Selection selection `json:"selection"`
}

type lifecyclePolicy struct {
	Rules []lifecycleRule `json:"rules"`
}

func invalidLifecycle(reason string) error {
	return invalidParameter("%s'%s'", lifecycleMessage, reason)
}

// parseLifecycle reads and checks a lifecycle policy, returning its rules in
// priority order.
func parseLifecycle(text *string) ([]lifecycleRule, error) {
	if text == nil || strings.TrimSpace(*text) == "" || len(*text) > maxLifecycle {
		return nil, invalidLifecycle("Lifecycle policy text must be 1 to 30720 characters long")
	}

	var p lifecyclePolicy
	d := json.NewDecoder(bytes.NewReader([]byte(*text)))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return nil, invalidLifecycle("Lifecycle policy validation failure: " + err.Error())
	}

	if len(p.Rules) == 0 {
		return nil, invalidLifecycle("Lifecycle policy validation failure: rules are required")
	}

	priorities := map[int]bool{}
	for i := range p.Rules {
		if err := checkRule(&p.Rules[i], priorities); err != nil {
			return nil, err
		}
	}

	slices.SortFunc(p.Rules, func(a, b lifecycleRule) int { return *a.RulePriority - *b.RulePriority })
	for i := range len(p.Rules) - 1 {
		if p.Rules[i].Selection.TagStatus == statusAny {
			return nil, invalidLifecycle("Lifecycle policy validation failure: a rule with tagStatus any must " +
				"have the highest rulePriority")
		}
	}

	return p.Rules, nil
}

func checkRule(r *lifecycleRule, priorities map[int]bool) error {
	sel := &r.Selection
	switch {
	case r.RulePriority == nil || *r.RulePriority < 1:
		return invalidLifecycle("Lifecycle policy validation failure: rulePriority must be a positive integer")
	case priorities[*r.RulePriority]:
		return invalidLifecycle("Lifecycle policy validation failure: rulePriority values must be unique")
	case r.Action.Type != "expire":
		return invalidLifecycle("Lifecycle policy validation failure: action type must be expire")
	case sel.CountNumber == nil || *sel.CountNumber < 1:
		return invalidLifecycle("Lifecycle policy validation failure: countNumber must be a positive integer")
	case !slices.Contains([]string{statusTagged, statusUntagged, statusAny}, sel.TagStatus):
		return invalidLifecycle("Lifecycle policy validation failure: tagStatus must be tagged, untagged, or any")
	case sel.TagStatus == statusTagged && (len(sel.TagPrefixList) == 0) == (len(sel.TagPatternList) == 0):
		return invalidLifecycle("Lifecycle policy validation failure: a tagged rule needs a tagPrefixList or a " +
			"tagPatternList")
	case sel.TagStatus != statusTagged && (len(sel.TagPrefixList) > 0 || len(sel.TagPatternList) > 0):
		return invalidLifecycle("Lifecycle policy validation failure: only tagged rules take tag lists")
	case sel.CountType == countMoreThan && sel.CountUnit != "":
		return invalidLifecycle("Lifecycle policy validation failure: imageCountMoreThan takes no countUnit")
	case sel.CountType == sincePushed && sel.CountUnit != "days":
		return invalidLifecycle("Lifecycle policy validation failure: sinceImagePushed needs countUnit days")
	case sel.CountType != countMoreThan && sel.CountType != sincePushed:
		return invalidLifecycle("Lifecycle policy validation failure: countType must be imageCountMoreThan or " +
			"sinceImagePushed")
	}

	priorities[*r.RulePriority] = true
	return nil
}

// matches reports whether img's tags pass a rule's selection: every prefix or
// pattern must match one of them.
func (sel *selection) matches(img *image) bool {
	switch sel.TagStatus {
	case statusUntagged:
		return len(img.Tags) == 0
	case statusAny:
		return true
	}

	if len(img.Tags) == 0 {
		return false
	}

	for _, p := range sel.TagPrefixList {
		if !slices.ContainsFunc(img.Tags, func(t string) bool { return strings.HasPrefix(t, p) }) {
			return false
		}
	}

	for _, p := range sel.TagPatternList {
		if !slices.ContainsFunc(img.Tags, func(t string) bool { ok, _ := path.Match(p, t); return ok }) {
			return false
		}
	}

	return true
}

// expire applies r's lifecycle policy: rules claim the images they select in
// priority order, so a later rule never sees an earlier rule's images, and
// each expires those past its count. Images an index lists are kept.
func (s *Service) expire(tx storage.WriteTx, scope request.Scope, r *repository) error {
	if r.Lifecycle == "" {
		return nil
	}

	rules, err := parseLifecycle(&r.Lifecycle)
	if err != nil {
		return err
	}

	remaining, err := images(tx, scope, r)
	if err != nil {
		return err
	}

	now := s.now().UTC()
	r.Evaluated = now
	for i := range rules {
		sel := &rules[i].Selection
		var chosen []*image
		remaining = slices.DeleteFunc(remaining, func(img *image) bool {
			if sel.matches(img) {
				chosen = append(chosen, img)
				return true
			}

			return false
		})

		// Newest first.
		slices.SortFunc(chosen, func(a, b *image) int { return b.Pushed.Compare(a.Pushed) })
		cutoff := now.Add(-time.Duration(*sel.CountNumber) * day)
		for j, img := range chosen {
			keep := int64(j) < *sel.CountNumber
			if sel.CountType == sincePushed {
				keep = img.Pushed.After(cutoff)
			}

			if keep {
				continue
			}

			if _, deleteErr := deleteImage(tx, scope, r, img); deleteErr != nil {
				return deleteErr
			}
		}
	}

	return nil
}

func lifecycleNotFound(m *request.Metadata, r *repository) error {
	return awserror.New("LifecyclePolicyNotFoundException", "Lifecycle policy does not exist for the repository "+
		"with name '"+r.Name+"' in the registry with id '"+m.AccountID+"'")
}

func (s *Service) PutLifecyclePolicy(
	ctx context.Context,
	in *api.PutLifecyclePolicyRequest,
) (*api.PutLifecyclePolicyResponse, error) {
	if _, err := parseLifecycle(in.LifecyclePolicyText); err != nil {
		return nil, err
	}

	var out *api.PutLifecyclePolicyResponse
	err := s.update(ctx, in.RegistryId, in.RepositoryName,
		func(tx storage.WriteTx, m *request.Metadata, r *repository) error {
			r.Lifecycle = *in.LifecyclePolicyText
			out = &api.PutLifecyclePolicyResponse{
				RegistryId: new(m.AccountID), RepositoryName: new(r.Name), LifecyclePolicyText: new(r.Lifecycle),
			}
			return s.expire(tx, m.Scope, r)
		})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) GetLifecyclePolicy(
	ctx context.Context,
	in *api.GetLifecyclePolicyRequest,
) (*api.GetLifecyclePolicyResponse, error) {
	var out *api.GetLifecyclePolicyResponse
	err := s.view(ctx, in.RegistryId, in.RepositoryName,
		func(_ storage.ReadTx, m *request.Metadata, r *repository) error {
			if r.Lifecycle == "" {
				return lifecycleNotFound(m, r)
			}

			out = &api.GetLifecyclePolicyResponse{
				RegistryId: new(m.AccountID), RepositoryName: new(r.Name), LifecyclePolicyText: new(r.Lifecycle),
				LastEvaluatedAt: seconds(r.Evaluated),
			}
			return nil
		})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeleteLifecyclePolicy(
	ctx context.Context,
	in *api.DeleteLifecyclePolicyRequest,
) (*api.DeleteLifecyclePolicyResponse, error) {
	var out *api.DeleteLifecyclePolicyResponse
	err := s.update(ctx, in.RegistryId, in.RepositoryName,
		func(_ storage.WriteTx, m *request.Metadata, r *repository) error {
			if r.Lifecycle == "" {
				return lifecycleNotFound(m, r)
			}

			out = &api.DeleteLifecyclePolicyResponse{
				RegistryId: new(m.AccountID), RepositoryName: new(r.Name), LifecyclePolicyText: new(r.Lifecycle),
				LastEvaluatedAt: seconds(r.Evaluated),
			}
			r.Lifecycle, r.Evaluated = "", time.Time{}
			return nil
		})
	if err != nil {
		return nil, err
	}

	return out, nil
}

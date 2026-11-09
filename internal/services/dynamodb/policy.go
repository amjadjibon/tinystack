package dynamodb

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/iam/policy"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type resourcePolicy struct {
	Document string
	Revision string
}

func policyTableKey(ctx context.Context, arn string) (string, error) {
	parts := strings.SplitN(arn, ":", 6)
	m, err := request.Regional(ctx)
	if err != nil {
		return "", err
	}
	if len(parts) != 6 || !strings.HasPrefix(arn, "arn:aws:dynamodb:") || parts[3] != m.Region ||
		!strings.HasPrefix(parts[5], "table/") ||
		len(parts[4]) != 12 {
		return "", awserror.Validation("Invalid ResourceArn")
	}

	name := strings.Split(parts[5], "/")[1]
	if !tableName.MatchString(name) {
		return "", awserror.Validation("Invalid table ARN")
	}
	return parts[4] + "/" + parts[3] + "/dynamodb/" + name, nil
}

func tablePolicy(t *table, arn string) (*resourcePolicy, error) {
	if arn != common.Value(t.Description.TableArn) && arn != common.Value(t.Description.LatestStreamArn) {
		return nil, awserror.New("ResourceNotFoundException", "Requested resource not found")
	}

	p := t.ResourcePolicies[arn]
	return &p, nil
}

func policyMissing() error {
	return awserror.New("PolicyNotFoundException", "Policy or expected revision not found")
}

func (s *Service) PutResourcePolicy(
	ctx context.Context,
	in *api.PutResourcePolicyInput,
) (*api.PutResourcePolicyOutput, error) {
	arn, doc := common.Value(in.ResourceArn), common.Value(in.Policy)
	key, err := policyTableKey(ctx, arn)
	if err != nil {
		return nil, err
	}
	if len(doc) > 20*1024 {
		return nil, awserror.Validation("Policy exceeds 20 KB")
	}
	if guardErr := policy.ValidateResource(doc); guardErr != nil {
		return nil, awserror.Validation("%s", guardErr)
	}
	revision := fmt.Sprintf("%x", sha256.Sum256([]byte(doc)))
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}
		p, findErr := tablePolicy(t, arn)
		if findErr != nil {
			return findErr
		}
		if in.ExpectedRevisionId != nil && *in.ExpectedRevisionId != p.Revision &&
			(*in.ExpectedRevisionId != "NO_POLICY" || p.Document != "") {
			return policyMissing()
		}
		if t.ResourcePolicies == nil {
			t.ResourcePolicies = map[string]resourcePolicy{}
		}
		t.ResourcePolicies[arn] = resourcePolicy{Document: doc, Revision: revision}
		return save(tx, key, t)
	})
	return &api.PutResourcePolicyOutput{RevisionId: new(revision)}, err
}

func (s *Service) GetResourcePolicy(
	ctx context.Context,
	in *api.GetResourcePolicyInput,
) (*api.GetResourcePolicyOutput, error) {
	arn := common.Value(in.ResourceArn)
	key, err := policyTableKey(ctx, arn)
	if err != nil {
		return nil, err
	}
	out := &api.GetResourcePolicyOutput{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		t, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}
		p, findErr := tablePolicy(t, arn)
		if findErr != nil {
			return findErr
		}
		if p.Document == "" {
			return policyMissing()
		}
		out.Policy, out.RevisionId = new(p.Document), new(p.Revision)
		return nil
	})
	return out, err
}

func (s *Service) DeleteResourcePolicy(
	ctx context.Context,
	in *api.DeleteResourcePolicyInput,
) (*api.DeleteResourcePolicyOutput, error) {
	arn := common.Value(in.ResourceArn)
	key, err := policyTableKey(ctx, arn)
	if err != nil {
		return nil, err
	}
	out := &api.DeleteResourcePolicyOutput{}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}
		p, findErr := tablePolicy(t, arn)
		if findErr != nil {
			return findErr
		}
		if in.ExpectedRevisionId != nil && (p.Document == "" || *in.ExpectedRevisionId != p.Revision) {
			return policyMissing()
		}
		if p.Document != "" {
			out.RevisionId = new(p.Revision)
		}
		delete(t.ResourcePolicies, arn)
		return save(tx, key, t)
	})
	return out, err
}

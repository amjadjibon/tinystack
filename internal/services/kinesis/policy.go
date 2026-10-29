package kinesis

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/kinesis"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/iam/policy"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var policyPrincipal = regexp.MustCompile(`^([0-9]{12}|arn:aws:iam::[0-9]{12}:(root|user/[^*?]+|role/[^*?]+))$`)

func checkPolicy(document string, consumer bool) error {
	if len(document) > 20*1024 {
		return invalidArgument("Policy exceeds 20 KB")
	}

	if err := policy.ValidateResource(document); err != nil {
		return invalidArgument("%s", err)
	}

	var doc struct{ Statement json.RawMessage }
	_ = json.Unmarshal([]byte(document), &doc)
	var statements []map[string]json.RawMessage
	if json.Unmarshal(doc.Statement, &statements) != nil {
		var one map[string]json.RawMessage
		_ = json.Unmarshal(doc.Statement, &one)
		statements = []map[string]json.RawMessage{one}
	}

	allowed := []string{
		"kinesis:DescribeStream",
		"kinesis:DescribeStreamSummary",
		"kinesis:GetRecords",
		"kinesis:GetShardIterator",
		"kinesis:ListShards",
		"kinesis:PutRecord",
		"kinesis:PutRecords",
	}

	if consumer {
		allowed = []string{"kinesis:DescribeStreamConsumer", "kinesis:SubscribeToShard"}
	}

	for _, st := range statements {
		var principals map[string]json.RawMessage
		if json.Unmarshal(st["Principal"], &principals) != nil || len(principals) != 1 || principals["AWS"] == nil {
			return invalidArgument("Only concrete AWS principals are supported")
		}

		for _, p := range policyStrings(principals["AWS"]) {
			if !policyPrincipal.MatchString(p) {
				return invalidArgument("Unsupported principal %s", p)
			}
		}

		if st["NotAction"] != nil {
			return invalidArgument("NotAction is unsupported")
		}

		for _, action := range policyStrings(st["Action"]) {
			if !slices.Contains(allowed, action) {
				return invalidArgument("Action %s is not shareable for this resource", action)
			}
		}
	}

	return nil
}

func policyStrings(raw json.RawMessage) []string {
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		return many
	}

	var one string
	_ = json.Unmarshal(raw, &one)
	return []string{one}
}

// policyTarget requires an exact stream or consumer ARN and keeps policy management owner-only.
func policyTarget(ctx context.Context, arn string) (string, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return "", err
	}

	streamARN, _, _ := strings.Cut(arn, "/consumer/")
	scope, _, ok := ParseStreamARN(streamARN)
	if !ok || scope.Region != m.Region {
		return "", invalidArgument("Invalid ResourceARN")
	}

	if scope.AccountID != m.AccountID {
		return "", awserror.New("AccessDeniedException", "Only the resource owner may manage its policy")
	}

	return streamARN, nil
}

func resourcePolicy(st *stream, arn string) (*string, error) {
	if !strings.Contains(arn, "/consumer/") {
		return &st.Policy, nil
	}

	for i := range st.Consumers {
		if st.Consumers[i].ARN == arn {
			return &st.Consumers[i].Policy, nil
		}
	}

	return nil, awserror.New("ResourceNotFoundException", "Consumer does not exist")
}

func (s *Service) PutResourcePolicy(
	ctx context.Context,
	in *api.PutResourcePolicyInput,
) (*api.PutResourcePolicyOutput, error) {
	arn := common.Value(in.ResourceARN)
	streamARN, err := policyTarget(ctx, arn)
	if err != nil {
		return nil, err
	}

	if policyErr := checkPolicy(common.Value(in.Policy), strings.Contains(arn, "/consumer/")); policyErr != nil {
		return nil, policyErr
	}

	err = s.change(ctx, nil, &streamARN, func(_ storage.WriteTx, _ request.Scope, st *stream) error {
		p, findErr := resourcePolicy(st, arn)
		if findErr != nil {
			return findErr
		}

		*p = *in.Policy
		return nil
	})
	return &api.PutResourcePolicyOutput{}, err
}

func (s *Service) GetResourcePolicy(
	ctx context.Context,
	in *api.GetResourcePolicyInput,
) (*api.GetResourcePolicyOutput, error) {
	arn := common.Value(in.ResourceARN)
	streamARN, err := policyTarget(ctx, arn)
	if err != nil {
		return nil, err
	}

	out := &api.GetResourcePolicyOutput{}
	err = s.view(ctx, nil, &streamARN, func(_ storage.ReadTx, _ request.Scope, st *stream) error {
		p, findErr := resourcePolicy(st, arn)
		if findErr != nil {
			return findErr
		}

		out.Policy = new(*p)
		if *p == "" {
			out.Policy = new("{}")
		}

		return nil
	})
	return out, err
}

func (s *Service) DeleteResourcePolicy(
	ctx context.Context,
	in *api.DeleteResourcePolicyInput,
) (*api.DeleteResourcePolicyOutput, error) {
	arn := common.Value(in.ResourceARN)
	streamARN, err := policyTarget(ctx, arn)
	if err != nil {
		return nil, err
	}

	err = s.change(ctx, nil, &streamARN, func(_ storage.WriteTx, _ request.Scope, st *stream) error {
		p, findErr := resourcePolicy(st, arn)
		if findErr != nil {
			return findErr
		}

		*p = ""
		return nil
	})
	return &api.DeleteResourcePolicyOutput{}, err
}

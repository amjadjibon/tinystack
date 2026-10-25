package s3

import (
	"context"
	"encoding/json"
	"net/http"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	iampolicy "github.com/amjadjibon/tinystack/internal/services/iam/policy"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// maxPolicyBytes is the largest bucket policy S3 accepts.
const maxPolicyBytes = 20 * 1024

// bucket reads the caller's bucket metadata.
func (s *Service) bucket(ctx context.Context, bucket, owner *string) (bucketMeta, error) {
	var meta bucketMeta
	m, err := request.Regional(ctx)
	if err != nil {
		return meta, err
	}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		key, resolveErr := resolve(tx, m, bucket, owner)
		if resolveErr != nil {
			return resolveErr
		}

		_, readErr := readJSON(tx, key, &meta)
		return readErr
	})
	return meta, err
}

// statement contains the fields GetBucketPolicyStatus uses to detect public access.
type statement struct {
	Effect    string
	Principal json.RawMessage
	Condition json.RawMessage
}

// parsePolicy checks that a policy is a JSON object with statements.
func parsePolicy(policy string) ([]statement, error) {
	malformed := fail(http.StatusBadRequest, "MalformedPolicy",
		"Policies must be valid JSON and the first byte must be '{'")
	if len(policy) > maxPolicyBytes {
		return nil, fail(http.StatusBadRequest, "MalformedPolicy", "Policy exceeds the maximum allowed document size.")
	}

	var doc struct {
		Statement json.RawMessage
	}

	if err := json.Unmarshal([]byte(policy), &doc); err != nil || policy == "" || policy[0] != '{' {
		return nil, malformed
	}

	var statements []statement
	if err := json.Unmarshal(doc.Statement, &statements); err != nil {
		var one statement
		if err = json.Unmarshal(doc.Statement, &one); err != nil {
			return nil, fail(http.StatusBadRequest, "MalformedPolicy", "Missing required field Statement")
		}

		statements = []statement{one}
	}

	if len(statements) == 0 {
		return nil, fail(http.StatusBadRequest, "MalformedPolicy", "Missing required field Statement")
	}

	return statements, nil
}

// public reports whether a statement allows everyone without conditions.
func (st *statement) public() bool {
	if st.Effect != "Allow" || len(st.Condition) > 0 {
		return false
	}

	var everyone string
	if json.Unmarshal(st.Principal, &everyone) == nil {
		return everyone == "*"
	}

	var principals struct {
		AWS json.RawMessage
	}

	if json.Unmarshal(st.Principal, &principals) != nil {
		return false
	}

	var one string
	if json.Unmarshal(principals.AWS, &one) == nil {
		return one == "*"
	}

	var many []string
	_ = json.Unmarshal(principals.AWS, &many)
	for _, p := range many {
		if p == "*" {
			return true
		}
	}

	return false
}

func noSuchPolicy() error {
	return fail(http.StatusNotFound, "NoSuchBucketPolicy", "The bucket policy does not exist")
}

func (s *Service) PutBucketPolicy(
	ctx context.Context,
	in *api.PutBucketPolicyRequest,
) (*api.PutBucketPolicyOutput, error) {
	policy := common.Value(in.Policy)
	statements, err := parsePolicy(policy)
	if err != nil {
		return nil, err
	}

	if err = iampolicy.ValidateResource(policy); err != nil {
		return nil, fail(http.StatusBadRequest, "MalformedPolicy", err.Error())
	}

	err = s.editBucket(ctx, in.Bucket, in.ExpectedBucketOwner, func(meta *bucketMeta) error {
		if b := meta.PublicAccessBlock; b != nil && common.Value(b.BlockPublicPolicy) && public(statements) {
			return fail(http.StatusForbidden, "AccessDenied", "User is not authorized to perform: s3:PutBucketPolicy "+
				"on resource because public policies are blocked by the BlockPublicPolicy block public access setting.")
		}

		meta.Policy = policy
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.PutBucketPolicyOutput{}, nil
}

func (s *Service) GetBucketPolicy(
	ctx context.Context,
	in *api.GetBucketPolicyRequest,
) (*api.GetBucketPolicyOutput, error) {
	meta, err := s.bucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	if meta.Policy == "" {
		return nil, noSuchPolicy()
	}

	return &api.GetBucketPolicyOutput{Policy: new(meta.Policy)}, nil
}

func (s *Service) DeleteBucketPolicy(
	ctx context.Context,
	in *api.DeleteBucketPolicyRequest,
) (*api.DeleteBucketPolicyOutput, error) {
	err := s.updateBucket(ctx, in.Bucket, in.ExpectedBucketOwner, func(meta *bucketMeta) { meta.Policy = "" })
	if err != nil {
		return nil, err
	}

	return &api.DeleteBucketPolicyOutput{}, nil
}

// GetBucketPolicyStatus reports a bucket as public when a statement allows
// every principal without conditions.
func (s *Service) GetBucketPolicyStatus(
	ctx context.Context,
	in *api.GetBucketPolicyStatusRequest,
) (*api.GetBucketPolicyStatusOutput, error) {
	meta, err := s.bucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	if meta.Policy == "" {
		return nil, noSuchPolicy()
	}

	statements, err := parsePolicy(meta.Policy)
	if err != nil {
		return nil, err
	}

	return &api.GetBucketPolicyStatusOutput{PolicyStatus: &api.PolicyStatus{IsPublic: new(public(statements))}}, nil
}

// public reports whether a policy allows everyone.
func public(statements []statement) bool {
	for i := range statements {
		if statements[i].public() {
			return true
		}
	}

	return false
}

// PolicyIsPublic validates a resource policy and shares S3's public-policy analysis with S3 Control.
func PolicyIsPublic(document string) (bool, error) {
	statements, err := parsePolicy(document)
	if err != nil {
		return false, err
	}

	if err := iampolicy.ValidateResource(document); err != nil {
		return false, fail(400, "MalformedPolicy", err.Error())
	}

	return public(statements), nil
}

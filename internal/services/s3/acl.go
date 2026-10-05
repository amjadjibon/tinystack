package s3

import (
	"context"
	"net/http"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
)

const (
	fullControl   = "FULL_CONTROL"
	canonicalUser = "CanonicalUser"
)

// ownerACL is the only ACL TinyStack keeps on buckets and objects: the owner
// has full control.
func ownerACL(account string) (*api.Owner, []api.Grant) {
	id := canonicalID(account)
	return &api.Owner{ID: new(id)}, []api.Grant{{
		Grantee: &api.Grantee{ID: new(id), Type: new(canonicalUser)}, Permission: new(fullControl),
	}}
}

// acl is an ACL a PutBucketAcl or PutObjectAcl request sets: a canned ACL,
// grant headers, or an access control policy.
type acl struct {
	canned      *string
	fullControl *string
	policy      *api.AccessControlPolicy
}

// private reports whether the ACL gives only the account's owner full
// control, which is the one ACL TinyStack supports. Canned names are the ones
// that mean that for the resource.
func (a *acl) private(account string, canned ...string) (bool, error) {
	if count(a.canned != nil, a.fullControl != nil, a.policy != nil) != 1 {
		return false, fail(http.StatusBadRequest, "InvalidRequest",
			"Specify exactly one of a canned ACL, grant headers, or an access control policy")
	}

	id := canonicalID(account)
	switch {
	case a.canned != nil:
		for _, name := range canned {
			if *a.canned == name {
				return true, nil
			}
		}

		return false, nil
	case a.fullControl != nil:
		return *a.fullControl == `id="`+id+`"`, nil
	}

	if o := a.policy.Owner; o != nil && common.Value(o.ID) != id {
		return false, fail(http.StatusForbidden, "AccessDenied", "Access Denied")
	}

	if len(a.policy.Grants) == 0 {
		return false, nil
	}

	for _, g := range a.policy.Grants {
		if g.Grantee == nil || common.Value(g.Grantee.Type) != canonicalUser || common.Value(g.Grantee.ID) != id ||
			common.Value(g.Permission) != fullControl {
			return false, nil
		}
	}

	return true, nil
}

// unsupportedACL is the error for an ACL that is not private: buckets that
// enforce owner ownership have ACLs disabled, and TinyStack keeps no others.
func unsupportedACL(ownership string) error {
	if ownership == ownerEnforced {
		return fail(http.StatusBadRequest, "AccessControlListNotSupported", "The bucket does not allow ACLs")
	}

	return fail(http.StatusNotImplemented, "NotImplemented",
		"TinyStack only supports ACLs that give the owner full control")
}

func (s *Service) GetBucketAcl(ctx context.Context, in *api.GetBucketAclRequest) (*api.GetBucketAclOutput, error) {
	if _, err := s.bucket(ctx, in.Bucket, in.ExpectedBucketOwner); err != nil {
		return nil, err
	}

	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	owner, grants := ownerACL(m.AccountID)
	return &api.GetBucketAclOutput{Owner: owner, Grants: grants}, nil
}

// PutBucketAcl accepts only the private ACL, which every bucket already has.
func (s *Service) PutBucketAcl(ctx context.Context, in *api.PutBucketAclRequest) (*api.PutBucketAclOutput, error) {
	meta, err := s.bucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	a := acl{canned: in.ACL, fullControl: in.GrantFullControl, policy: in.AccessControlPolicy}
	private, err := a.private(m.AccountID, "private")
	if err != nil {
		return nil, err
	}

	if !private {
		return nil, unsupportedACL(meta.Ownership)
	}

	return &api.PutBucketAclOutput{}, nil
}

func (s *Service) GetObjectAcl(ctx context.Context, in *api.GetObjectAclRequest) (*api.GetObjectAclOutput, error) {
	if _, err := s.HeadObject(ctx, &api.HeadObjectRequest{
		Bucket: in.Bucket, Key: in.Key, VersionId: in.VersionId, ExpectedBucketOwner: in.ExpectedBucketOwner,
	}); err != nil {
		return nil, err
	}

	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	owner, grants := ownerACL(m.AccountID)
	return &api.GetObjectAclOutput{Owner: owner, Grants: grants}, nil
}

// PutObjectAcl accepts only the private ACL, which every object already has.
func (s *Service) PutObjectAcl(ctx context.Context, in *api.PutObjectAclRequest) (*api.PutObjectAclOutput, error) {
	meta, err := s.bucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	if _, err = s.HeadObject(ctx, &api.HeadObjectRequest{
		Bucket: in.Bucket, Key: in.Key, VersionId: in.VersionId, ExpectedBucketOwner: in.ExpectedBucketOwner,
	}); err != nil {
		return nil, err
	}

	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	a := acl{canned: in.ACL, fullControl: in.GrantFullControl, policy: in.AccessControlPolicy}
	private, err := a.private(m.AccountID, "private", "bucket-owner-full-control")
	if err != nil {
		return nil, err
	}

	if !private {
		return nil, unsupportedACL(meta.Ownership)
	}

	return &api.PutObjectAclOutput{}, nil
}

package signer

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/signer"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var (
	statementPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)
	principalPattern = regexp.MustCompile(
		`^(\d{12}|arn:aws(?:-us-gov|-cn)?:iam::\d{12}:` +
			`(?:root|(?:role|user)/[A-Za-z0-9+=,.@_/-]+)|[a-z0-9.-]+\.amazonaws\.com)$`,
	)
)

// AddProfilePermission appends a statement with optimistic revision checks.
func (s *Service) AddProfilePermission(
	ctx context.Context,
	in *api.AddProfilePermissionRequest,
) (*api.AddProfilePermissionResponse, error) {
	if !statementPattern.MatchString(common.Value(in.StatementId)) ||
		!principalPattern.MatchString(common.Value(in.Principal)) {
		return nil, invalid("Invalid StatementId or Principal.")
	}

	if !slices.Contains(
		[]string{
			"signer:StartSigningJob",
			"signer:GetSigningProfile",
			"signer:SignPayload",
			"signer:RevokeSignature",
			"signer:*",
		},
		common.Value(in.Action),
	) {
		return nil, invalid("Invalid permission Action.")
	}

	r, err := s.change(ctx, in.ProfileName, func(r *profile) error {
		if in.RevisionId != nil && *in.RevisionId != r.Revision {
			return conflict()
		}

		if in.ProfileVersion != nil {
			if _, ok := r.Versions[*in.ProfileVersion]; !ok {
				return missing()
			}
		}

		for _, p := range r.Permissions {
			if *p.StatementId == *in.StatementId {
				return conflict()
			}
		}

		r.Permissions = append(r.Permissions, api.Permission{
			Action: in.Action, Principal: in.Principal,
			StatementId: in.StatementId, ProfileVersion: in.ProfileVersion,
		})
		slices.SortFunc(r.Permissions, func(a, b api.Permission) int {
			return strings.Compare(*a.StatementId, *b.StatementId)
		})
		r.Revision = common.RandomID(16)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.AddProfilePermissionResponse{RevisionId: new(r.Revision)}, nil
}

// RemoveProfilePermission removes an existing statement using the latest revision ID.
func (s *Service) RemoveProfilePermission(
	ctx context.Context,
	in *api.RemoveProfilePermissionRequest,
) (*api.RemoveProfilePermissionResponse, error) {
	r, err := s.change(ctx, in.ProfileName, func(r *profile) error {
		if common.Value(in.RevisionId) != r.Revision {
			return conflict()
		}

		for i, p := range r.Permissions {
			if *p.StatementId == common.Value(in.StatementId) {
				r.Permissions = slices.Delete(r.Permissions, i, i+1)
				r.Revision = common.RandomID(16)
				return nil
			}
		}

		return missing()
	})
	if err != nil {
		return nil, err
	}

	return &api.RemoveProfilePermissionResponse{RevisionId: new(r.Revision)}, nil
}

// ListProfilePermissions returns statements and the current policy revision ID.
func (s *Service) ListProfilePermissions(
	ctx context.Context,
	in *api.ListProfilePermissionsRequest,
) (*api.ListProfilePermissionsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.ListProfilePermissionsResponse
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := read(tx, scope, in.ProfileName)
		if e != nil {
			return e
		}

		page, next, e := common.Page(r.Permissions, func(p api.Permission) string { return *p.StatementId },
			in.NextToken, 25, invalid("Invalid NextToken."))
		if e != nil {
			return e
		}

		body, e := json.Marshal(r.Permissions)
		if e != nil {
			return e
		}

		out = &api.ListProfilePermissionsResponse{
			Permissions: page, NextToken: next, RevisionId: new(r.Revision),
			PolicySizeBytes: new(int64(len(body))),
		}

		return nil
	})
	return out, err
}

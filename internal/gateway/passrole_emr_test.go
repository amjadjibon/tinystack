package gateway

import (
	"context"
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/emr"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/iam"
)

func TestEMRPassedRoles(t *testing.T) {
	const account = "000000000000"
	const role = "arn:aws:iam::" + account + ":role/emr-service"
	const nodeRole = "arn:aws:iam::" + account + ":role/emr-node"
	s := &Server{}
	s.UseInstanceProfiles(func(_ context.Context, a, ref string) (iam.InstanceProfile, bool, error) {
		if a != account || ref != "nodes" {
			return iam.InstanceProfile{}, false, nil
		}

		return iam.InstanceProfile{RoleARN: nodeRole}, true, nil
	})
	ctx := request.WithMetadata(t.Context(), request.Metadata{AccountID: account})
	for _, ref := range []string{role, "emr-service"} {
		targets, err := s.passedRoles(
			ctx,
			"emr",
			"RunJobFlow",
			&api.RunJobFlowInput{ServiceRole: new(ref), JobFlowRole: new("nodes")},
		)
		if err != nil || len(targets) != 2 {
			t.Fatal(targets, err)
		}

		if targets[0].ARN != role || targets[0].Values["iam:passedtoservice"][0] != "elasticmapreduce.amazonaws.com" ||
			targets[1].ARN != nodeRole || targets[1].Values["iam:passedtoservice"][0] != "ec2.amazonaws.com" {
			t.Fatal(targets)
		}
	}

	_, err := s.passedRoles(ctx, "emr", "RunJobFlow", &api.RunJobFlowInput{JobFlowRole: new("missing")})
	if err == nil {
		t.Fatal("accepted missing instance profile")
	}
}

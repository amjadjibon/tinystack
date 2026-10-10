package cloudformation

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/backup"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/backup"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestBackupRetagCompensation(t *testing.T) {
	for _, member := range []string{memberBackupVaultTags, memberBackupPlanTags} {
		for _, failure := range []string{"", "ListTags", opUntagResource, opTagResource, "canceled", "limit", "restore"} {
			t.Run(member+"/"+failure, func(t *testing.T) {
				scope := request.Scope{AccountID: "000000000000", Region: "us-east-1"}
				ctx := request.WithMetadata(context.Background(), request.Metadata{Scope: scope})
				s := backup.New(&memory.Store{})
				vault, err := s.CreateBackupVault(ctx, &api.CreateBackupVaultInput{BackupVaultName: new("primary")})
				if err != nil {
					t.Fatal(err)
				}
				arn, attribute := *vault.BackupVaultArn, memberBackupVaultARN
				if member == memberBackupPlanTags {
					plan, createErr := s.CreateBackupPlan(ctx, &api.CreateBackupPlanInput{
						BackupPlan: &api.BackupPlanInput{BackupPlanName: new("daily"), Rules: []api.BackupRuleInput{
							{RuleName: new("daily"), TargetBackupVaultName: new("primary")},
						}},
					})
					if createErr != nil {
						t.Fatal(createErr)
					}
					arn, attribute = *plan.BackupPlanArn, memberBackupPlanARN
				}
				old := map[string]string{}
				for i := range 48 {
					old[fmt.Sprintf("key%02d", i)] = "original"
				}
				live := maps.Clone(old)
				live["key00"] = "changed-outside-stack"
				live["external1"], live["external2"] = "keep1", "keep2"
				if _, err = s.TagResource(ctx, &api.TagResourceInput{ResourceArn: &arn, Tags: live}); err != nil {
					t.Fatal(err)
				}
				next := maps.Clone(old)
				delete(next, "key00")
				next["replacement"] = "new"
				next["key01"] = "updated"
				if failure == "limit" {
					next["one-too-many"] = "new"
				}
				callCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				descriptor := api.Descriptor(s)
				injected := awserror.New("ServiceUnavailableException", "injected failure")
				tagCalls := 0
				for _, operation := range []string{"ListTags", opUntagResource, opTagResource} {
					op := descriptor.Operations[operation]
					invoke := op.Invoke
					op.Invoke = func(ctx context.Context, input any) (any, error) {
						if operation == opTagResource {
							tagCalls++
							if failure == "restore" ||
								tagCalls == 1 && (failure == "canceled" || failure == opTagResource) {
								if failure == "canceled" {
									cancel()
								}
								return nil, injected
							}
						} else if failure == operation {
							return nil, injected
						}
						return invoke(ctx, input)
					}
					descriptor.Operations[operation] = op
				}
				c := &client{caller: catalog.NewCaller(descriptor), scope: scope}
				r := &resource{Properties: props{member: tagMap(old)}, Attributes: map[string]any{attribute: arn}}
				err = backupRetag(callCtx, c, r, props{member: tagMap(next)}, member, attribute)
				want := maps.Clone(live)
				switch failure {
				case "":
					if err != nil {
						t.Fatal(err)
					}
					delete(want, "key00")
					want["replacement"], want["key01"] = "new", "updated"
				case "limit":
					if awserror.From(err).Code != "LimitExceededException" {
						t.Fatalf("expected atomic limit failure: %v", err)
					}
				case "restore":
					if err == nil || !strings.Contains(failureReason(err), "restoring removed Backup tags failed") {
						t.Fatalf("compensation failure not reported: %v", err)
					}
					delete(want, "key00")
				default:
					if !errors.Is(err, injected) {
						t.Fatalf("original error lost: %v", err)
					}
				}
				got, err := s.ListTags(ctx, &api.ListTagsInput{ResourceArn: &arn})
				if err != nil || !reflect.DeepEqual(got.Tags, want) {
					t.Fatalf("tags after reconciliation: %+v, want %v, err %v", got, want, err)
				}
			})
		}
	}
}

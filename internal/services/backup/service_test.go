package backup

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/backup"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func backupContext() context.Context {
	return request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: "000000000000", Region: "us-east-1"}},
	)
}

func planInput() *api.BackupPlanInput {
	return &api.BackupPlanInput{BackupPlanName: new("daily"), Rules: []api.BackupRuleInput{
		{
			RuleName:              new("daily-rule"),
			TargetBackupVaultName: new("primary"),
			ScheduleExpression: new(
				"cron(0 5 ? * * *)",
			),
			StartWindowMinutes:      new(int64(60)),
			CompletionWindowMinutes: new(int64(120)),
			Lifecycle: &api.Lifecycle{
				MoveToColdStorageAfterDays: new(int64(30)),
				DeleteAfterDays:            new(int64(120)),
			},
			RecoveryPointTags: map[string]string{"owner": "platform"},
		},
	}}
}

func selectionInput() *api.BackupSelection {
	return &api.BackupSelection{
		SelectionName: new("daily-resources"),
		IamRoleArn:    new("arn:aws:iam::000000000000:role/backup"),
		Resources: []string{
			"arn:aws:s3:::data-bucket",
		},
		ListOfTags: []api.Condition{
			{ConditionType: new("STRINGEQUALS"), ConditionKey: new("backup"), ConditionValue: new("daily")},
		},
	}
}

func errorCode(t *testing.T, err error, want string) {
	t.Helper()
	var e *awserror.Error
	if !errors.As(err, &e) || e.Code != want {
		t.Fatalf("want %s, got %v", want, err)
	}
}

func fixture(t *testing.T) (*Service, *api.CreateBackupPlanOutput) {
	t.Helper()
	ctx := backupContext()
	s := New(&memory.Store{})
	if _, err := s.CreateBackupVault(ctx, &api.CreateBackupVaultInput{BackupVaultName: new("primary")}); err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateBackupPlan(
		ctx,
		&api.CreateBackupPlanInput{BackupPlan: planInput(), CreatorRequestId: new("plan-token")},
	)
	if err != nil {
		t.Fatal(err)
	}
	return s, p
}

func TestInvalidUpdatesKeepEntirePlan(t *testing.T) {
	s, p := fixture(t)
	ctx := backupContext()
	before, _ := s.GetBackupPlan(ctx, &api.GetBackupPlanInput{BackupPlanId: p.BackupPlanId})
	versions, _ := s.ListBackupPlanVersions(ctx, &api.ListBackupPlanVersionsInput{BackupPlanId: p.BackupPlanId})
	cases := []struct {
		edit func(*api.BackupPlanInput)
		name string
	}{
		{name: "missing name", edit: func(p *api.BackupPlanInput) { p.BackupPlanName = nil }},
		{name: "empty name", edit: func(p *api.BackupPlanInput) { p.BackupPlanName = new("") }},
		{name: "no rules", edit: func(p *api.BackupPlanInput) { p.Rules = nil }},
		{name: "missing rule name", edit: func(p *api.BackupPlanInput) { p.Rules[0].RuleName = nil }},
		{name: "missing target", edit: func(p *api.BackupPlanInput) { p.Rules[0].TargetBackupVaultName = nil }},
		{
			name: "foreign target",
			edit: func(p *api.BackupPlanInput) { p.Rules[0].TargetBackupVaultName = new("unknown") },
		},
		{name: "empty cron", edit: func(p *api.BackupPlanInput) { p.Rules[0].ScheduleExpression = new("") }},
		{
			name: "bad cron",
			edit: func(p *api.BackupPlanInput) { p.Rules[0].ScheduleExpression = new("cron(90 5 ? * * *)") },
		},
		{name: "rate", edit: func(p *api.BackupPlanInput) { p.Rules[0].ScheduleExpression = new("rate(1 day)") }},
		{name: "start zero", edit: func(p *api.BackupPlanInput) { p.Rules[0].StartWindowMinutes = new(int64(0)) }},
		{
			name: "start high",
			edit: func(p *api.BackupPlanInput) { p.Rules[0].StartWindowMinutes = new(int64(52560001)) },
		},
		{
			name: "complete zero",
			edit: func(p *api.BackupPlanInput) { p.Rules[0].CompletionWindowMinutes = new(int64(0)) },
		},
		{name: "empty lifecycle", edit: func(p *api.BackupPlanInput) { p.Rules[0].Lifecycle = &api.Lifecycle{} }},
		{
			name: "negative retention",
			edit: func(p *api.BackupPlanInput) { p.Rules[0].Lifecycle.DeleteAfterDays = new(int64(-1)) },
		},
		{
			name: "cold minimum",
			edit: func(p *api.BackupPlanInput) { p.Rules[0].Lifecycle.DeleteAfterDays = new(int64(119)) },
		},
		{
			name: "reserved tag",
			edit: func(p *api.BackupPlanInput) { p.Rules[0].RecoveryPointTags = map[string]string{"aws:bad": "x"} },
		},
		{name: "duplicate rules", edit: func(p *api.BackupPlanInput) { p.Rules = append(p.Rules, p.Rules[0]) }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			in := planInput()
			test.edit(in)
			if _, err := s.UpdateBackupPlan(
				ctx,
				&api.UpdateBackupPlanInput{BackupPlanId: p.BackupPlanId, BackupPlan: in},
			); err == nil {
				t.Fatal("invalid update succeeded")
			}
			after, _ := s.GetBackupPlan(ctx, &api.GetBackupPlanInput{BackupPlanId: p.BackupPlanId})
			history, _ := s.ListBackupPlanVersions(ctx, &api.ListBackupPlanVersionsInput{BackupPlanId: p.BackupPlanId})
			if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(versions, history) {
				t.Fatal("invalid update changed state or history")
			}
		})
	}
	updated := planInput()
	updated.Rules[0].Lifecycle.DeleteAfterDays = new(int64(200))
	u, err := s.UpdateBackupPlan(ctx, &api.UpdateBackupPlanInput{BackupPlanId: p.BackupPlanId, BackupPlan: updated})
	if err != nil {
		t.Fatal(err)
	}
	old, _ := s.GetBackupPlan(ctx, &api.GetBackupPlanInput{BackupPlanId: p.BackupPlanId, VersionId: p.VersionId})
	now, _ := s.GetBackupPlan(ctx, &api.GetBackupPlanInput{BackupPlanId: p.BackupPlanId})
	if *u.VersionId == *p.VersionId || !reflect.DeepEqual(old, before) ||
		*old.BackupPlan.Rules[0].RuleId != *now.BackupPlan.Rules[0].RuleId {
		t.Fatal("versions not immutable or rule identity changed")
	}
	// Mutating returned objects cannot mutate serialized storage.
	now.BackupPlan.Rules[0].RecoveryPointTags["owner"] = "mutated"
	got, _ := s.GetBackupPlan(ctx, &api.GetBackupPlanInput{BackupPlanId: p.BackupPlanId})
	if got.BackupPlan.Rules[0].RecoveryPointTags["owner"] != "platform" {
		t.Fatal("returned mutable storage")
	}
}

func TestConcurrentCreatorsAndTags(t *testing.T) {
	s, p := fixture(t)
	ctx := backupContext()
	var wg sync.WaitGroup
	for i := range 24 {
		wg.Go(func() {
			got, err := s.CreateBackupPlan(
				ctx,
				&api.CreateBackupPlanInput{BackupPlan: planInput(), CreatorRequestId: new("plan-token")},
			)
			if err != nil || *got.BackupPlanId != *p.BackupPlanId {
				t.Errorf("idempotency: %+v %v", got, err)
			}
			_, err = s.CreateBackupSelection(
				ctx,
				&api.CreateBackupSelectionInput{
					BackupPlanId:     p.BackupPlanId,
					BackupSelection:  selectionInput(),
					CreatorRequestId: new("selection-token"),
				},
			)
			if err != nil {
				t.Error(err)
			}
			_, err = s.TagResource(
				ctx,
				&api.TagResourceInput{
					ResourceArn: p.BackupPlanArn,
					Tags:        map[string]string{fmt.Sprintf("key-%02d", i): "value"},
				},
			)
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	tags, err := s.ListTags(ctx, &api.ListTagsInput{ResourceArn: p.BackupPlanArn})
	if err != nil || len(tags.Tags) != 24 {
		t.Fatalf("lost merge: %+v %v", tags, err)
	}
	sels, err := s.ListBackupSelections(ctx, &api.ListBackupSelectionsInput{BackupPlanId: p.BackupPlanId})
	if err != nil || len(sels.BackupSelectionsList) != 1 {
		t.Fatalf("duplicate selection: %+v %v", sels, err)
	}
	extra := map[string]string{}
	for i := range 27 {
		extra[fmt.Sprintf("extra-%02d", i)] = "value"
	}
	_, err = s.TagResource(ctx, &api.TagResourceInput{ResourceArn: p.BackupPlanArn, Tags: extra})
	errorCode(t, err, "LimitExceededException")
	after, _ := s.ListTags(ctx, &api.ListTagsInput{ResourceArn: p.BackupPlanArn})
	if !reflect.DeepEqual(tags, after) {
		t.Fatal("invalid tags partially merged")
	}
}

func TestSelectionValidationAndDeletion(t *testing.T) {
	s, p := fixture(t)
	ctx := backupContext()
	edits := []func(*api.BackupSelection){
		func(s *api.BackupSelection) { s.SelectionName = nil }, func(s *api.BackupSelection) { s.IamRoleArn = nil },
		func(s *api.BackupSelection) { s.IamRoleArn = new("arn:aws:iam::000000000000:user/backup") },
		func(s *api.BackupSelection) { s.Resources = nil; s.ListOfTags = nil },
		func(s *api.BackupSelection) { s.Resources = []string{"arn:aws:s3:::*"} },
		func(s *api.BackupSelection) { s.Resources = append(s.Resources, s.Resources[0]) },
		func(s *api.BackupSelection) { s.ListOfTags[0].ConditionType = new("StringEquals") },
		func(s *api.BackupSelection) { s.ListOfTags[0].ConditionValue = nil },
		func(s *api.BackupSelection) { s.ListOfTags[0].ConditionKey = new("") },
		func(s *api.BackupSelection) { s.ListOfTags = append(s.ListOfTags, s.ListOfTags[0]) },
	}
	for _, edit := range edits {
		in := selectionInput()
		edit(in)
		if _, err := s.CreateBackupSelection(
			ctx,
			&api.CreateBackupSelectionInput{BackupPlanId: p.BackupPlanId, BackupSelection: in},
		); err == nil {
			t.Fatal("invalid selection accepted")
		}
	}
	list, _ := s.ListBackupSelections(ctx, &api.ListBackupSelectionsInput{BackupPlanId: p.BackupPlanId})
	if len(list.BackupSelectionsList) != 0 {
		t.Fatal("invalid selection published")
	}
	sel, err := s.CreateBackupSelection(
		ctx,
		&api.CreateBackupSelectionInput{BackupPlanId: p.BackupPlanId, BackupSelection: selectionInput()},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DeleteBackupPlan(ctx, &api.DeleteBackupPlanInput{BackupPlanId: p.BackupPlanId})
	errorCode(t, err, "InvalidRequestException")
	_, err = s.DeleteBackupSelection(
		ctx,
		&api.DeleteBackupSelectionInput{BackupPlanId: p.BackupPlanId, SelectionId: sel.SelectionId},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DeleteBackupSelection(
		ctx,
		&api.DeleteBackupSelectionInput{BackupPlanId: p.BackupPlanId, SelectionId: sel.SelectionId},
	)
	errorCode(t, err, "ResourceNotFoundException")
	_, err = s.DeleteBackupPlan(ctx, &api.DeleteBackupPlanInput{BackupPlanId: p.BackupPlanId})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.GetBackupPlan(ctx, &api.GetBackupPlanInput{BackupPlanId: p.BackupPlanId, VersionId: p.VersionId})
	errorCode(t, err, "ResourceNotFoundException")
	deleted, _ := s.ListBackupPlans(ctx, &api.ListBackupPlansInput{IncludeDeleted: new(true)})
	if len(deleted.BackupPlansList) != 1 || deleted.BackupPlansList[0].DeletionDate == nil {
		t.Fatal("deleted plan metadata lost")
	}
}

func TestBackupLimitsAndOptionalMetadata(t *testing.T) {
	s, p := fixture(t)
	ctx := backupContext()
	for _, name := range []string{"", "a", "bad.name", "name/escape"} {
		_, err := s.CreateBackupVault(ctx, &api.CreateBackupVaultInput{BackupVaultName: new(name)})
		errorCode(t, err, "InvalidParameterValueException")
	}
	_, err := s.CreateBackupVault(ctx, &api.CreateBackupVaultInput{})
	errorCode(t, err, "MissingParameterValueException")
	for _, key := range []string{
		"", "arn:aws:kms:eu-west-1:000000000000:key/1234abcd-12ab-34cd-56ef-1234567890ab",
		"arn:aws:kms:us-east-1:111111111111:key/1234abcd-12ab-34cd-56ef-1234567890ab",
		"arn:aws:kms:us-east-1:000000000000:alias/aws/backup",
	} {
		_, err = s.CreateBackupVault(
			ctx,
			&api.CreateBackupVaultInput{BackupVaultName: new("invalid-key"), EncryptionKeyArn: new(key)},
		)
		errorCode(t, err, "InvalidParameterValueException")
	}
	v, err := s.DescribeBackupVault(ctx, &api.DescribeBackupVaultInput{BackupVaultName: new("primary")})
	if err != nil || v.EncryptionKeyArn != nil || v.CreatorRequestId != nil || *v.NumberOfRecoveryPoints != 0 {
		t.Fatalf("fabricated defaults: %+v %v", v, err)
	}
	// Only nested names/target are required; omitted optional fields stay absent
	// except for the modeled default schedule.
	minimal := &api.BackupPlanInput{
		BackupPlanName: new("minimal"),
		Rules:          []api.BackupRuleInput{{RuleName: new("rule"), TargetBackupVaultName: new("primary")}},
	}
	q, err := s.CreateBackupPlan(ctx, &api.CreateBackupPlanInput{BackupPlan: minimal})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetBackupPlan(ctx, &api.GetBackupPlanInput{BackupPlanId: q.BackupPlanId})
	if err != nil {
		t.Fatal(err)
	}
	r := got.BackupPlan.Rules[0]
	if *r.ScheduleExpression != "cron(0 5 ? * * *)" || r.StartWindowMinutes != nil ||
		r.CompletionWindowMinutes != nil ||
		r.Lifecycle != nil {
		t.Fatal("optional metadata changed")
	}
	oversized := selectionInput()
	oversized.ListOfTags = nil
	oversized.Resources = nil
	for i := range 501 {
		oversized.Resources = append(oversized.Resources, fmt.Sprintf("arn:aws:s3:::bucket-%d", i))
	}
	_, err = s.CreateBackupSelection(
		ctx,
		&api.CreateBackupSelectionInput{BackupPlanId: p.BackupPlanId, BackupSelection: oversized},
	)
	errorCode(t, err, "InvalidParameterValueException")
	oversized = selectionInput()
	oversized.ListOfTags = nil
	for i := range 31 {
		oversized.ListOfTags = append(
			oversized.ListOfTags,
			api.Condition{
				ConditionType:  new("STRINGEQUALS"),
				ConditionKey:   new(fmt.Sprintf("key-%d", i)),
				ConditionValue: new("value"),
			},
		)
	}
	_, err = s.CreateBackupSelection(
		ctx,
		&api.CreateBackupSelectionInput{BackupPlanId: p.BackupPlanId, BackupSelection: oversized},
	)
	errorCode(t, err, "InvalidParameterValueException")
	for i := range 10 {
		in := selectionInput()
		in.SelectionName = new(fmt.Sprintf("selection-%d", i))
		if _, err = s.CreateBackupSelection(
			ctx,
			&api.CreateBackupSelectionInput{BackupPlanId: p.BackupPlanId, BackupSelection: in},
		); err != nil {
			t.Fatal(err)
		}
	}
	_, err = s.CreateBackupSelection(
		ctx,
		&api.CreateBackupSelectionInput{BackupPlanId: p.BackupPlanId, BackupSelection: selectionInput()},
	)
	errorCode(t, err, "LimitExceededException")
	_, err = s.UntagResource(
		ctx,
		&api.UntagResourceInput{ResourceArn: p.BackupPlanArn, TagKeyList: []string{"same", "same"}},
	)
	errorCode(t, err, "InvalidParameterValueException")
	_, err = s.TagResource(
		ctx,
		&api.TagResourceInput{
			ResourceArn: new("arn:aws:backup:us-east-1:000000000000:backup-plan:unknown"),
			Tags:        map[string]string{"valid": "value"},
		},
	)
	errorCode(t, err, "ResourceNotFoundException")
}

func TestBackupConcurrentVaultDuplicate(t *testing.T) {
	s := New(&memory.Store{})
	ctx := backupContext()
	var wg sync.WaitGroup
	results := make(chan error, 16)
	for range 16 {
		wg.Go(func() {
			_, err := s.CreateBackupVault(ctx, &api.CreateBackupVaultInput{BackupVaultName: new("same-name")})
			results <- err
		})
	}
	wg.Wait()
	close(results)
	created := 0
	for err := range results {
		if err == nil {
			created++
		} else {
			errorCode(t, err, "AlreadyExistsException")
		}
	}
	if created != 1 {
		t.Fatalf("created %d same-named vaults", created)
	}
}

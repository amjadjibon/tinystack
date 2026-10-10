package backup

import (
	"errors"
	"regexp"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/backup"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/schedule"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var (
	vaultName = regexp.MustCompile(`^[a-zA-Z0-9_-]{2,50}$`)
	shortName = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,50}$`)
	planName  = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,200}$`)
	keyARN    = regexp.MustCompile(
		`^arn:aws:kms:([a-z0-9-]+):(\d{12}):key/(` +
			`[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}|mrk-[a-fA-F0-9]{32})$`,
	)
	roleARN     = regexp.MustCompile(`^arn:aws:iam::\d{12}:role/[A-Za-z0-9+=,.@_/-]+$`)
	resourceARN = regexp.MustCompile(`^arn:aws:[a-z0-9-]+:[a-z0-9-]*:(\d{12})?:[^\s*?]+$`)
)

func requiredName(p *string, pattern *regexp.Regexp, label string) error {
	if p == nil {
		return failure("MissingParameterValueException", label+" is required")
	}
	if !pattern.MatchString(*p) {
		return invalid(label + " has an invalid format")
	}
	return nil
}

func tokenOK(token *string) error {
	if token != nil && !shortName.MatchString(*token) {
		return invalid("CreatorRequestId must contain 1-50 alphanumeric, dash, underscore or dot characters")
	}
	return nil
}

func tagError(err error) error {
	if errors.Is(err, tagging.ErrLimit) {
		return failure("LimitExceededException", err.Error())
	}
	return invalid(err.Error())
}

func tagsOK(tags map[string]string) error {
	if err := tagging.CheckSet(tags); err != nil {
		return tagError(err)
	}
	return nil
}

func validatePlan(in *api.BackupPlanInput) error {
	if in == nil {
		return failure("MissingParameterValueException", "BackupPlan is required")
	}
	if err := requiredName(in.BackupPlanName, planName, "BackupPlanName"); err != nil {
		return err
	}
	if len(in.Rules) < 1 || len(in.Rules) > 10 {
		return invalid("Rules must contain 1-10 rules")
	}
	seen := map[string]bool{}
	for _, r := range in.Rules {
		if err := requiredName(r.RuleName, shortName, "RuleName"); err != nil {
			return err
		}
		if seen[*r.RuleName] {
			return invalid("RuleName must be unique within the plan")
		}
		seen[*r.RuleName] = true
		if err := requiredName(r.TargetBackupVaultName, vaultName, "TargetBackupVaultName"); err != nil {
			return err
		}
		if r.ScheduleExpression != nil {
			if !strings.HasPrefix(*r.ScheduleExpression, "cron(") {
				return invalid("ScheduleExpression must be a supported cron expression")
			}
			if _, err := schedule.Parse(*r.ScheduleExpression); err != nil {
				return invalid(err.Error())
			}
		}
		if r.StartWindowMinutes != nil && (*r.StartWindowMinutes < 60 || *r.StartWindowMinutes > 52560000) {
			return invalid("StartWindowMinutes must be between 60 and 52560000")
		}
		if r.CompletionWindowMinutes != nil &&
			(*r.CompletionWindowMinutes < 1 || *r.CompletionWindowMinutes > 52560000) {
			return invalid("CompletionWindowMinutes must be between 1 and 52560000")
		}
		if l := r.Lifecycle; l != nil {
			if l.MoveToColdStorageAfterDays == nil && l.DeleteAfterDays == nil {
				return invalid("Lifecycle must specify a retention or transition")
			}
			for _, n := range []*int64{l.MoveToColdStorageAfterDays, l.DeleteAfterDays} {
				if n != nil && (*n < 1 || *n > 36500) {
					return invalid("Lifecycle days must be between 1 and 36500")
				}
			}
			if l.MoveToColdStorageAfterDays != nil && l.DeleteAfterDays != nil &&
				*l.DeleteAfterDays < *l.MoveToColdStorageAfterDays+90 {
				return invalid("Cold storage requires at least 90 days before deletion")
			}
		}
		if len(r.RecoveryPointTags) > 10 {
			return invalid("RecoveryPointTags support at most 10 tags")
		}
		if err := tagsOK(r.RecoveryPointTags); err != nil {
			return invalid(err.Error())
		}
	}
	return nil
}

func planSnapshot(
	tx storage.ReadTx,
	scope request.Scope,
	in *api.BackupPlanInput,
	previous *api.BackupPlan,
) (api.BackupPlan, error) {
	out := api.BackupPlan{BackupPlanName: in.BackupPlanName, Rules: []api.BackupRule{}}
	for _, r := range in.Rules {
		if _, err := read(tx, scope, kindVault, *r.TargetBackupVaultName); err != nil {
			var apiErr *awserror.Error
			if errors.As(err, &apiErr) && apiErr.Code == "ResourceNotFoundException" {
				return out, invalid("TargetBackupVaultName must identify an existing regional vault")
			}
			return out, err
		}
		ruleID := id()
		if previous != nil {
			for _, old := range previous.Rules {
				if common.Value(old.RuleName) == *r.RuleName {
					ruleID = common.Value(old.RuleId)
				}
			}
		}
		out.Rules = append(out.Rules, api.BackupRule{
			RuleName: r.RuleName, TargetBackupVaultName: r.TargetBackupVaultName,
			ScheduleExpression: common.OrElse(r.ScheduleExpression, new("cron(0 5 ? * * *)")),
			StartWindowMinutes: r.StartWindowMinutes, CompletionWindowMinutes: r.CompletionWindowMinutes,
			Lifecycle: r.Lifecycle, RecoveryPointTags: r.RecoveryPointTags, RuleId: new(ruleID),
		})
	}
	return out, nil
}

func validateSelection(in *api.BackupSelection) error {
	if in == nil {
		return failure("MissingParameterValueException", "BackupSelection is required")
	}
	if err := requiredName(in.SelectionName, shortName, "SelectionName"); err != nil {
		return err
	}
	if err := requiredName(in.IamRoleArn, roleARN, "IamRoleArn"); err != nil {
		return err
	}
	role := *in.IamRoleArn
	roleName := role[strings.LastIndex(role, "/")+1:]
	if len(role) > 2048 || len(roleName) < 1 || len(roleName) > 64 || strings.Contains(role, "//") {
		return invalid("IamRoleArn must have a nonempty role name of at most 64 characters and a valid path")
	}
	if len(in.Resources) == 0 && len(in.ListOfTags) == 0 {
		return invalid("Specify explicit Resources or ListOfTags")
	}
	if len(in.Resources) > 500 || len(in.ListOfTags) > 30 {
		return invalid("Selections support at most 500 explicit ARNs and 30 tag selectors")
	}
	seen := map[string]bool{}
	for _, a := range in.Resources {
		if len(a) > 2048 || strings.ContainsAny(a, "\x00\r\n") || !resourceARN.MatchString(a) || seen[a] {
			return invalid("Resources must be unique explicit commercial resource ARNs without wildcards")
		}
		seen[a] = true
	}
	seen = map[string]bool{}
	for _, t := range in.ListOfTags {
		if t.ConditionType == nil || t.ConditionKey == nil || t.ConditionValue == nil {
			return failure(
				"MissingParameterValueException",
				"Tag selectors require ConditionType, ConditionKey and ConditionValue",
			)
		}
		if *t.ConditionType != "STRINGEQUALS" {
			return invalid("ListOfTags supports only STRINGEQUALS")
		}
		if err := tagsOK(map[string]string{*t.ConditionKey: *t.ConditionValue}); err != nil {
			return err
		}
		pair := *t.ConditionKey + "\x00" + *t.ConditionValue
		if seen[pair] {
			return invalid("Tag selectors must be unique")
		}
		seen[pair] = true
	}
	return nil
}

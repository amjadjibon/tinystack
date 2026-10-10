package cloudformation

import (
	"context"
	"maps"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
)

const (
	serviceBackup         = "backup"
	memberBackupPlanID    = "BackupPlanId"
	memberBackupPlan      = "BackupPlan"
	memberBackupSelection = "BackupSelection"
	memberBackupVaultName = "BackupVaultName"
	memberBackupPlanTags  = "BackupPlanTags"
	memberBackupVaultTags = "BackupVaultTags"
	memberBackupPlanARN   = "BackupPlanArn"
	memberBackupVaultARN  = "BackupVaultArn"
	memberSelectionID     = "SelectionId"
	memberVersionID       = "VersionId"
)

func init() {
	register("AWS::Backup::BackupVault", &resourceType{
		name:       memberBackupVaultName,
		properties: []string{memberBackupVaultName, memberBackupVaultTags, "EncryptionKeyArn"},
		replace: []string{
			memberBackupVaultName,
			"EncryptionKeyArn",
		},
		attributes: []string{memberBackupVaultARN, memberBackupVaultName},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			if _, err := backupTags(p, memberBackupVaultTags); err != nil {
				return "", nil, err
			}
			params := pick(map[string]any{}, p, memberBackupVaultName, memberBackupVaultTags, "EncryptionKeyArn")
			if !p.has(memberBackupVaultName) {
				params[memberBackupVaultName] = c.generate(50)
			}
			out, err := c.call(ctx, serviceBackup, "CreateBackupVault", params)
			if err != nil {
				return "", nil, err
			}
			return outText(out, memberBackupVaultName), out, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			err := backupRetag(ctx, c, r, p, memberBackupVaultTags, memberBackupVaultARN)
			return r.Attributes, err
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			return deleteBackup(ctx, c, "DeleteBackupVault", map[string]any{memberBackupVaultName: r.Physical})
		},
	})
	register("AWS::Backup::BackupPlan", &resourceType{
		properties: []string{
			memberBackupPlan,
			memberBackupPlanTags,
		},
		attributes: []string{memberBackupPlanID, memberBackupPlanARN, memberVersionID},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			plan, err := backupPlan(p)
			if err != nil {
				return "", nil, err
			}
			if _, tagErr := backupTags(p, memberBackupPlanTags); tagErr != nil {
				return "", nil, tagErr
			}
			params := pick(map[string]any{memberBackupPlan: plan}, p, memberBackupPlanTags)
			out, err := c.call(ctx, serviceBackup, "CreateBackupPlan", params)
			if err != nil {
				return "", nil, err
			}
			return outText(out, memberBackupPlanID), out, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			plan, err := backupPlan(p)
			if err != nil {
				return nil, err
			}
			// Validate tags before changing a plan version. The runner restores the
			// previous plan on failure; backupRetag compensates its own partial changes.
			if _, err = backupTags(p, memberBackupPlanTags); err != nil {
				return nil, err
			}
			out, err := c.call(
				ctx,
				serviceBackup,
				"UpdateBackupPlan",
				map[string]any{memberBackupPlanID: r.Physical, memberBackupPlan: plan},
			)
			if err != nil {
				return nil, err
			}
			if tagErr := backupRetag(ctx, c, r, p, memberBackupPlanTags, memberBackupPlanARN); tagErr != nil {
				return nil, tagErr
			}
			return out, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			return deleteBackup(ctx, c, "DeleteBackupPlan", map[string]any{memberBackupPlanID: r.Physical})
		},
	})
	register("AWS::Backup::BackupSelection", &resourceType{
		properties: []string{memberBackupPlanID, memberBackupSelection},
		attributes: []string{memberBackupPlanID, memberSelectionID},
		// Both properties require replacement in CloudFormation; there is no
		// UpdateBackupSelection operation. The runner creates before cleanup.
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			out, err := c.call(
				ctx,
				serviceBackup,
				"CreateBackupSelection",
				pick(map[string]any{}, p, memberBackupPlanID, memberBackupSelection),
			)
			if err != nil {
				return "", nil, err
			}
			return outText(out, memberSelectionID), out, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			return deleteBackup(
				ctx,
				c,
				"DeleteBackupSelection",
				map[string]any{
					memberBackupPlanID: props(r.Properties).str(memberBackupPlanID),
					memberSelectionID:  r.Physical,
				},
			)
		},
	})
}

func deleteBackup(ctx context.Context, c *client, operation string, params map[string]any) error {
	_, err := c.call(ctx, serviceBackup, operation, params)
	if gone(err, "ResourceNotFoundException") {
		return nil
	}
	return err
}

// CloudFormation uses BackupPlanRule and TargetBackupVault rather than the SDK
// names. Reject unexpected properties before converting those two names.
func backupPlan(p props) (map[string]any, error) {
	value, ok := p[memberBackupPlan].(map[string]any)
	if !ok {
		return nil, validation("BackupPlan must be an object")
	}
	for key := range value {
		if key != "BackupPlanName" && key != "BackupPlanRule" {
			return nil, validation("Unsupported BackupPlan property %s", key)
		}
	}
	out := maps.Clone(value)
	rules, ok := value["BackupPlanRule"].([]any)
	if !ok {
		return nil, validation("BackupPlanRule must be an array")
	}
	converted := []any{}
	for _, item := range rules {
		r, ok := item.(map[string]any)
		if !ok {
			return nil, validation("BackupPlanRule must contain objects")
		}
		if _, exists := r["TargetBackupVaultName"]; exists {
			return nil, validation("Use TargetBackupVault in CloudFormation")
		}
		rule := maps.Clone(r)
		if target, exists := rule["TargetBackupVault"]; exists {
			rule["TargetBackupVaultName"] = target
			delete(rule, "TargetBackupVault")
		}
		converted = append(converted, rule)
	}
	delete(out, "BackupPlanRule")
	out["Rules"] = converted
	return out, nil
}

func backupTags(p props, member string) (map[string]string, error) {
	out := map[string]string{}
	if !p.has(member) {
		return out, nil
	}
	value, ok := p[member].(map[string]any)
	if !ok {
		return nil, validation("%s must be a string map", member)
	}
	for key, v := range value {
		text, ok := v.(string)
		if !ok {
			return nil, validation("%s must have string values", member)
		}
		out[key] = text
	}
	if err := tagging.CheckSet(out); err != nil {
		return nil, validation("%s", err.Error())
	}
	return out, nil
}

func backupRetag(ctx context.Context, c *client, r *resource, p props, member, attribute string) error {
	tags, err := backupTags(p, member)
	if err != nil {
		return err
	}
	old, err := backupTags(props(r.Properties), member)
	if err != nil {
		return err
	}
	set, removed := tagChanges(old, tags)
	target := r.Attributes[attribute]
	// Failed handlers retain their old properties in the runner. Snapshot only
	// the live values we remove, so an unsuccessful merge can restore them even
	// when they differ from the template. Unrelated tags are never removed.
	restore := map[string]any{}
	if len(removed) > 0 && len(set) > 0 {
		live, readErr := c.call(ctx, serviceBackup, "ListTags", map[string]any{memberResourceArn: target})
		if readErr != nil {
			return readErr
		}
		values, _ := live[memberTags].(map[string]any)
		for _, key := range removed {
			name, _ := key.(string)
			if value, exists := values[name]; exists {
				restore[name] = value
			}
		}
	}
	// Free capacity before merging replacements at the service's 50-tag limit.
	if len(removed) > 0 {
		if _, err = c.call(
			ctx,
			serviceBackup,
			opUntagResource,
			map[string]any{memberResourceArn: target, "TagKeyList": removed},
		); err != nil {
			return err
		}
	}
	if len(set) > 0 {
		_, err = c.call(
			ctx,
			serviceBackup,
			opTagResource,
			map[string]any{memberResourceArn: target, memberTags: tagMap(set)},
		)
		// TagResource validates and merges atomically, so only removals need
		// compensation. Finish it even if the original call's context was canceled.
		if err != nil && len(restore) > 0 {
			if _, restoreErr := c.call(context.WithoutCancel(ctx), serviceBackup, opTagResource,
				map[string]any{memberResourceArn: target, memberTags: restore}); restoreErr != nil {
				failure := awserror.From(err)
				failure.Message += "; restoring removed Backup tags failed: " + restoreErr.Error()
				return failure
			}
		}
	}
	return err
}

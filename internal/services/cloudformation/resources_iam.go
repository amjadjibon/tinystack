package cloudformation

import (
	"context"
	"slices"
	"strings"
)

// principals are the identities a policy resource attaches to: the
// property naming them, their member name, and the suffix of their
// operations (AttachRolePolicy, PutUserPolicy).
var principals = []struct{ property, member, kind string }{
	{propertyRoles, memberRoleName, "Role"},
	{propertyUsers, "UserName", "User"},
	{propertyGroups, memberGroupName, "Group"},
}

// maxPolicyVersions is how many versions IAM keeps of a managed policy.
const maxPolicyVersions = 5

func init() {
	register("AWS::IAM::ManagedPolicy", &resourceType{
		name: propertyManagedPolicyName,
		properties: []string{
			propertyManagedPolicyName,
			memberPath,
			memberDescription,
			memberPolicyDocument,
			propertyRoles,
			propertyUsers,
			propertyGroups,
		},
		attributes: []string{memberPolicyArn, memberPolicyID, memberDefaultVersionID},
		replace:    []string{propertyManagedPolicyName, memberPath, memberDescription},
		iam:        true,
		create:     createManagedPolicy,
		update:     updateManagedPolicy,
		remove:     removeManagedPolicy,
	})
	register("AWS::IAM::Policy", &resourceType{
		properties: []string{memberPolicyName, memberPolicyDocument, propertyRoles, propertyUsers, propertyGroups},
		replace:    []string{memberPolicyName},
		iam:        true,
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			return c.generate(128), nil, inlinePolicy(ctx, c, props{}, p)
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			return nil, inlinePolicy(ctx, c, r.Properties, p)
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			return inlinePolicy(ctx, c, r.Properties, props{})
		},
	})
}

// attachments returns the identities a policy resource names by kind.
func attachments(p props) map[string][]string {
	out := map[string][]string{}
	for _, k := range principals {
		out[k.kind] = p.strings(k.property)
	}

	return out
}

// attach attaches a managed policy to the identities p names and detaches
// it from those only old names.
func attach(ctx context.Context, c *client, arn string, old, p props) error {
	before, after := attachments(old), attachments(p)
	for _, k := range principals {
		for _, name := range before[k.kind] {
			if slices.Contains(after[k.kind], name) {
				continue
			}

			if _, err := c.call(ctx, "iam", "Detach"+k.kind+"Policy", map[string]any{
				k.member: name, memberPolicyArn: arn,
			}); err != nil && !gone(err, "NoSuchEntityException") {
				return err
			}
		}

		for _, name := range after[k.kind] {
			if slices.Contains(before[k.kind], name) {
				continue
			}

			if _, err := c.call(ctx, "iam", "Attach"+k.kind+"Policy", map[string]any{
				k.member: name, memberPolicyArn: arn,
			}); err != nil {
				return err
			}
		}
	}

	return nil
}

func createManagedPolicy(
	ctx context.Context, c *client, p props,
) (physical string, attributes map[string]any, err error) {
	params := pick(map[string]any{memberPolicyName: c.nameOf(p, propertyManagedPolicyName, 128)}, p,
		memberPath, memberDescription)
	params[memberPolicyDocument] = p.document(memberPolicyDocument)
	out, err := c.call(ctx, "iam", "CreatePolicy", params)
	if err != nil {
		return "", nil, err
	}

	arn := outText(out, "Policy", memberArn)
	attributes = map[string]any{
		memberPolicyArn: arn, memberPolicyID: outText(out, "Policy", memberPolicyID),
		memberDefaultVersionID: outText(out, "Policy", memberDefaultVersionID),
	}
	return arn, attributes, attach(ctx, c, arn, props{}, p)
}

func updateManagedPolicy(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
	attributes := r.Attributes
	if p.changed(r.Properties, memberPolicyDocument) {
		versions, err := policyVersions(ctx, c, r.Physical)
		if err != nil {
			return nil, err
		}

		if len(versions) >= maxPolicyVersions {
			err = deletePolicyVersion(ctx, c, r.Physical, versions[len(versions)-1])
			if err != nil {
				return nil, err
			}
		}

		out, err := c.call(ctx, "iam", "CreatePolicyVersion", map[string]any{
			memberPolicyArn: r.Physical, memberPolicyDocument: p.document(memberPolicyDocument), "SetAsDefault": true,
		})
		if err != nil {
			return nil, err
		}

		attributes = map[string]any{
			memberPolicyArn: r.Physical, memberPolicyID: r.Attributes[memberPolicyID],
			memberDefaultVersionID: outText(out, "PolicyVersion", "VersionId"),
		}
	}

	return attributes, attach(ctx, c, r.Physical, r.Properties, p)
}

// policyVersions returns the IDs of a managed policy's versions that are not
// its default, newest first.
func policyVersions(ctx context.Context, c *client, arn string) ([]string, error) {
	out, err := c.call(ctx, "iam", "ListPolicyVersions", map[string]any{memberPolicyArn: arn})
	if err != nil {
		return nil, err
	}

	var ids []string
	list, _ := out["Versions"].([]any)
	for _, v := range list {
		m, _ := v.(map[string]any)
		if isDefault, _ := m["IsDefaultVersion"].(bool); !isDefault {
			ids = append(ids, outText(m, "VersionId"))
		}
	}

	// Version IDs are v1, v2, ...; the newest has the highest number.
	slices.SortFunc(ids, func(a, b string) int {
		if len(a) != len(b) {
			return len(b) - len(a)
		}

		return strings.Compare(b, a)
	})
	return ids, nil
}

func deletePolicyVersion(ctx context.Context, c *client, arn, id string) error {
	_, err := c.call(ctx, "iam", "DeletePolicyVersion", map[string]any{memberPolicyArn: arn, "VersionId": id})
	return err
}

func removeManagedPolicy(ctx context.Context, c *client, r *resource) error {
	err := deleteManagedPolicy(ctx, c, r)
	if gone(err, "NoSuchEntityException") {
		return nil
	}

	return err
}

// deleteManagedPolicy detaches a managed policy and deletes its versions and
// then the policy, as IAM requires.
func deleteManagedPolicy(ctx context.Context, c *client, r *resource) error {
	if err := attach(ctx, c, r.Physical, r.Properties, props{}); err != nil {
		return err
	}

	versions, err := policyVersions(ctx, c, r.Physical)
	if err != nil {
		return err
	}

	for _, id := range versions {
		if deleteErr := deletePolicyVersion(ctx, c, r.Physical, id); deleteErr != nil {
			return deleteErr
		}
	}

	_, err = c.call(ctx, "iam", "DeletePolicy", map[string]any{memberPolicyArn: r.Physical})
	return err
}

// inlinePolicy puts an inline policy on the identities p names and deletes
// it from those only old names; a renamed policy is a new resource.
func inlinePolicy(ctx context.Context, c *client, old, p props) error {
	before, after := attachments(old), attachments(p)
	document := p.document(memberPolicyDocument)
	for _, k := range principals {
		for _, name := range before[k.kind] {
			if slices.Contains(after[k.kind], name) {
				continue
			}

			if _, err := c.call(ctx, "iam", "Delete"+k.kind+"Policy", map[string]any{
				k.member: name, memberPolicyName: old.str(memberPolicyName),
			}); err != nil && !gone(err, "NoSuchEntityException") {
				return err
			}
		}

		for _, name := range after[k.kind] {
			if slices.Contains(before[k.kind], name) && !p.changed(old, memberPolicyDocument) {
				continue
			}

			if _, err := c.call(ctx, "iam", "Put"+k.kind+"Policy", map[string]any{
				k.member: name, memberPolicyName: p.str(memberPolicyName), memberPolicyDocument: document,
			}); err != nil {
				return err
			}
		}
	}

	return nil
}

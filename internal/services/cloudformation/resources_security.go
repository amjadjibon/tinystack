package cloudformation

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"maps"
	"math/big"
	"slices"
	"strings"
)

func init() {
	register("AWS::IAM::Role", &resourceType{
		name: memberRoleName,
		properties: []string{
			memberRoleName,
			memberPath,
			propertyAssumeRolePolicyDocument,
			memberDescription,
			"MaxSessionDuration",
			"ManagedPolicyArns",
			"Policies",
			memberTags,
		},
		attributes: []string{memberArn, "RoleId"},
		replace:    []string{memberRoleName, memberPath},
		iam:        true,
		create:     createRole,
		update:     updateRole,
		remove:     removeRole,
	})
	register("AWS::KMS::Key", &resourceType{
		properties: []string{
			memberDescription,
			"KeyPolicy",
			"KeySpec",
			"KeyUsage",
			memberEnabled,
			"EnableKeyRotation",
			"PendingWindowInDays",
			memberTags,
		},
		attributes: []string{memberArn, memberKeyID},
		replace:    []string{"KeySpec", "KeyUsage"},
		create:     createKey,
		update:     updateKey,
		remove: func(ctx context.Context, c *client, r *resource) error {
			params := pick(map[string]any{memberKeyID: r.Physical}, r.Properties, "PendingWindowInDays")
			_, err := c.call(ctx, "kms", "ScheduleKeyDeletion", params)
			if gone(err, "NotFoundException", "KMSInvalidStateException") {
				return nil
			}

			return err
		},
	})
	register("AWS::KMS::Alias", &resourceType{
		properties: []string{memberAliasName, "TargetKeyId"},
		replace:    []string{memberAliasName},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			_, err := c.call(ctx, "kms", "CreateAlias", pick(map[string]any{}, p, memberAliasName, "TargetKeyId"))
			return p.str(memberAliasName), nil, err
		},
		update: func(ctx context.Context, c *client, _ *resource, p props) (map[string]any, error) {
			_, err := c.call(ctx, "kms", "UpdateAlias", pick(map[string]any{}, p, memberAliasName, "TargetKeyId"))
			return nil, err
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "kms", "DeleteAlias", map[string]any{memberAliasName: r.Physical})
			if gone(err, "NotFoundException") {
				return nil
			}

			return err
		},
	})
	register("AWS::SecretsManager::Secret", &resourceType{
		name: memberName,
		properties: []string{
			memberName,
			memberDescription,
			"SecretString",
			"GenerateSecretString",
			memberKmsKeyID,
			memberTags,
		},
		attributes: []string{"Id"},
		replace:    []string{memberName},
		create:     createSecret,
		update:     updateSecret,
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "secretsmanager", "DeleteSecret", map[string]any{
				memberSecretID: r.Physical, "ForceDeleteWithoutRecovery": true,
			})
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
}

func createRole(ctx context.Context, c *client, p props) (physical string, attributes map[string]any, err error) {
	name := c.nameOf(p, memberRoleName, 64)
	params := pick(map[string]any{memberRoleName: name}, p, memberPath, memberDescription, "MaxSessionDuration")
	params[propertyAssumeRolePolicyDocument] = p.document(propertyAssumeRolePolicyDocument)
	if tags := p.tags(); len(tags) > 0 {
		params[memberTags] = tagList(tags, memberKey, memberValue)
	}

	out, err := c.call(ctx, "iam", "CreateRole", params)
	if err != nil {
		return "", nil, err
	}

	attributes = map[string]any{
		memberArn: outText(out, propertyRole, memberArn),
		"RoleId":  outText(out, propertyRole, "RoleId"),
	}
	return name, attributes, rolePolicies(ctx, c, name, props{}, p)
}

func updateRole(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
	old := props(r.Properties)
	if p.changed(old, propertyAssumeRolePolicyDocument) {
		if _, err := c.call(ctx, "iam", "UpdateAssumeRolePolicy", map[string]any{
			memberRoleName: r.Physical, "PolicyDocument": p.document(propertyAssumeRolePolicyDocument),
		}); err != nil {
			return nil, err
		}
	}

	if p.changed(old, memberDescription) || p.changed(old, "MaxSessionDuration") {
		params := pick(map[string]any{memberRoleName: r.Physical, memberDescription: p.str(memberDescription)}, p,
			"MaxSessionDuration")
		if _, err := c.call(ctx, "iam", "UpdateRole", params); err != nil {
			return nil, err
		}
	}

	if err := rolePolicies(ctx, c, r.Physical, old, p); err != nil {
		return nil, err
	}

	return r.Attributes, retag(ctx, c, old, p, &tagger{
		target:  map[string]any{memberRoleName: r.Physical},
		service: "iam", tag: "TagRole", untag: "UntagRole", key: memberKey, value: memberValue,
	})
}

// inlinePolicies reads a role's Policies property by name.
func inlinePolicies(p props) map[string]string {
	out := map[string]string{}
	for _, el := range p.list("Policies") {
		m, _ := el.(map[string]any)
		out[scalarText(m[memberPolicyName])] = props(m).document("PolicyDocument")
	}

	return out
}

// rolePolicies moves a role's inline and managed policies from old's to p's.
func rolePolicies(ctx context.Context, c *client, name string, old, p props) error {
	before, after := inlinePolicies(old), inlinePolicies(p)
	for _, policy := range slices.Sorted(maps.Keys(before)) {
		if _, kept := after[policy]; kept {
			continue
		}

		if _, err := c.call(ctx, "iam", "DeleteRolePolicy", map[string]any{
			memberRoleName: name, memberPolicyName: policy,
		}); err != nil && !gone(err, "NoSuchEntityException") {
			return err
		}
	}

	for _, policy := range slices.Sorted(maps.Keys(after)) {
		if prev, ok := before[policy]; ok && prev == after[policy] {
			continue
		}

		if _, err := c.call(ctx, "iam", "PutRolePolicy", map[string]any{
			memberRoleName: name, memberPolicyName: policy, "PolicyDocument": after[policy],
		}); err != nil {
			return err
		}
	}

	attached, wanted := old.strings("ManagedPolicyArns"), p.strings("ManagedPolicyArns")
	for _, arn := range attached {
		if slices.Contains(wanted, arn) {
			continue
		}

		if _, err := c.call(ctx, "iam", "DetachRolePolicy", map[string]any{
			memberRoleName: name, "PolicyArn": arn,
		}); err != nil && !gone(err, "NoSuchEntityException") {
			return err
		}
	}

	for _, arn := range wanted {
		if slices.Contains(attached, arn) {
			continue
		}

		if _, err := c.call(
			ctx,
			"iam",
			"AttachRolePolicy",
			map[string]any{memberRoleName: name, "PolicyArn": arn},
		); err != nil {
			return err
		}
	}

	return nil
}

func removeRole(ctx context.Context, c *client, r *resource) error {
	if err := rolePolicies(ctx, c, r.Physical, r.Properties, props{}); err != nil {
		if gone(err, "NoSuchEntityException") {
			return nil
		}

		return err
	}

	_, err := c.call(ctx, "iam", "DeleteRole", map[string]any{memberRoleName: r.Physical})
	if gone(err, "NoSuchEntityException") {
		return nil
	}

	return err
}

func createKey(ctx context.Context, c *client, p props) (physical string, attributes map[string]any, err error) {
	params := pick(map[string]any{}, p, memberDescription, "KeySpec", "KeyUsage")
	if p.has("KeyPolicy") {
		params[memberPolicy] = p.document("KeyPolicy")
	}

	if tags := p.tags(); len(tags) > 0 {
		params[memberTags] = tagList(tags, "TagKey", "TagValue")
	}

	out, err := c.call(ctx, "kms", "CreateKey", params)
	if err != nil {
		return "", nil, err
	}

	id := outText(out, "KeyMetadata", memberKeyID)
	attributes = map[string]any{memberArn: outText(out, "KeyMetadata", memberArn), memberKeyID: id}
	return id, attributes, keyState(ctx, c, id, props{}, p)
}

// keyState applies a key's Enabled and EnableKeyRotation when they change.
func keyState(ctx context.Context, c *client, id string, old, p props) error {
	if p.changed(old, "EnableKeyRotation") {
		operation := "DisableKeyRotation"
		if p.truth("EnableKeyRotation") {
			operation = "EnableKeyRotation"
		}

		if _, err := c.call(ctx, "kms", operation, map[string]any{memberKeyID: id}); err != nil {
			return err
		}
	}

	if !p.changed(old, memberEnabled) {
		return nil
	}

	operation := "EnableKey"
	if p.has(memberEnabled) && !p.truth(memberEnabled) {
		operation = "DisableKey"
	}

	_, err := c.call(ctx, "kms", operation, map[string]any{memberKeyID: id})
	return err
}

func updateKey(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
	old := props(r.Properties)
	if p.changed(old, memberDescription) {
		if _, err := c.call(ctx, "kms", "UpdateKeyDescription", map[string]any{
			memberKeyID: r.Physical, memberDescription: p.str(memberDescription),
		}); err != nil {
			return nil, err
		}
	}

	if p.changed(old, "KeyPolicy") && p.has("KeyPolicy") {
		if _, err := c.call(ctx, "kms", "PutKeyPolicy", map[string]any{
			memberKeyID: r.Physical, memberPolicyName: nameDefault, memberPolicy: p.document("KeyPolicy"),
		}); err != nil {
			return nil, err
		}
	}

	if err := keyState(ctx, c, r.Physical, old, p); err != nil {
		return nil, err
	}

	return r.Attributes, retag(ctx, c, old, p, &tagger{
		target:  map[string]any{memberKeyID: r.Physical},
		service: "kms", tag: opTagResource, untag: opUntagResource, key: "TagKey", value: "TagValue",
	})
}

func createSecret(ctx context.Context, c *client, p props) (physical string, attributes map[string]any, err error) {
	name := c.nameOf(p, memberName, 512)
	secret, err := secretString(p)
	if err != nil {
		return "", nil, err
	}

	params := pick(map[string]any{memberName: name, "SecretString": secret}, p, memberDescription, "KmsKeyId")
	if tags := p.tags(); len(tags) > 0 {
		params[memberTags] = tagList(tags, memberKey, memberValue)
	}

	out, err := c.call(ctx, "secretsmanager", "CreateSecret", params)
	if err != nil {
		return "", nil, err
	}

	arn := outText(out, "ARN")
	return arn, map[string]any{"Id": arn}, nil
}

func updateSecret(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
	old := props(r.Properties)
	params := map[string]any{memberSecretID: r.Physical}
	if p.changed(old, "SecretString") || p.changed(old, "GenerateSecretString") {
		secret, err := secretString(p)
		if err != nil {
			return nil, err
		}

		params["SecretString"] = secret
	}

	if p.changed(old, memberDescription) {
		params[memberDescription] = p.str(memberDescription)
	}

	if p.changed(old, "KmsKeyId") {
		params["KmsKeyId"] = p.str("KmsKeyId")
	}

	if len(params) > 1 {
		if _, err := c.call(ctx, "secretsmanager", "UpdateSecret", params); err != nil {
			return nil, err
		}
	}

	return r.Attributes, retag(ctx, c, old, p, &tagger{
		target:  map[string]any{memberSecretID: r.Physical},
		service: "secretsmanager", tag: opTagResource, untag: opUntagResource, key: memberKey, value: memberValue,
	})
}

const punctuation = "!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~"

// secretString returns a secret's SecretString, or generates one as
// GenerateSecretString describes.
func secretString(p props) (string, error) {
	spec, generate := p["GenerateSecretString"].(map[string]any)
	switch {
	case generate && p.has("SecretString"):
		return "", validation("You can't specify both SecretString and GenerateSecretString")
	case !generate:
		return p.str("SecretString"), nil
	}

	g := props(spec)
	for k := range g {
		if !slices.Contains([]string{
			"PasswordLength",
			"ExcludeCharacters",
			"ExcludeLowercase",
			"ExcludeUppercase",
			"ExcludeNumbers",
			"ExcludePunctuation",
			"IncludeSpace",
			"RequireEachIncludedType",
			"SecretStringTemplate",
			"GenerateStringKey",
		}, k) {
			return "", validation("TinyStack does not support the %s property of GenerateSecretString", k)
		}
	}

	password, err := randomPassword(g)
	if err != nil {
		return "", err
	}

	template, keyName := g.str("SecretStringTemplate"), g.str("GenerateStringKey")
	if template == "" && keyName == "" {
		return password, nil
	}

	if template == "" || keyName == "" {
		return "", validation("SecretStringTemplate and GenerateStringKey must be used together")
	}

	var doc map[string]any
	err = json.Unmarshal([]byte(template), &doc)
	if err != nil {
		return "", validation("SecretStringTemplate must be a JSON object")
	}

	doc[keyName] = password
	data, err := json.Marshal(doc)
	return string(data), err
}

func randomPassword(g props) (string, error) {
	length := 32
	if g.has("PasswordLength") {
		n, err := json.Number(g.str("PasswordLength")).Int64()
		if err != nil || n < 1 || n > 4096 {
			return "", validation("PasswordLength must be between 1 and 4096")
		}

		length = int(n)
	}

	var sets []string
	for _, set := range []struct{ chars, exclude string }{
		{"abcdefghijklmnopqrstuvwxyz", "ExcludeLowercase"},
		{"ABCDEFGHIJKLMNOPQRSTUVWXYZ", "ExcludeUppercase"},
		{"0123456789", "ExcludeNumbers"},
		{punctuation, "ExcludePunctuation"},
	} {
		if !g.truth(set.exclude) {
			sets = append(sets, set.chars)
		}
	}

	if g.truth("IncludeSpace") {
		sets = append(sets, " ")
	}

	excluded := g.str("ExcludeCharacters")
	for i, set := range sets {
		sets[i] = strings.Map(func(r rune) rune {
			if strings.ContainsRune(excluded, r) {
				return -1
			}

			return r
		}, set)
	}

	sets = slices.DeleteFunc(sets, func(s string) bool { return s == "" })
	if len(sets) == 0 {
		return "", validation("GenerateSecretString excludes every character")
	}

	out := make([]byte, 0, length)
	if !g.has("RequireEachIncludedType") || g.truth("RequireEachIncludedType") {
		for _, set := range sets {
			if len(out) < length {
				out = append(out, pickByte(set))
			}
		}
	}

	all := strings.Join(sets, "")
	for len(out) < length {
		out = append(out, pickByte(all))
	}

	for i := len(out) - 1; i > 0; i-- {
		j := int(randomInt(i + 1))
		out[i], out[j] = out[j], out[i]
	}

	return string(out), nil
}

func pickByte(set string) byte { return set[randomInt(len(set))] }

func randomInt(n int) int64 {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		panic(err)
	}

	return v.Int64()
}

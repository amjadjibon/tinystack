package cloudformation

import (
	"context"
	"maps"
	"net/url"
	"strings"
)

const (
	serviceIoT                   = "iot"
	memberThingName              = "ThingName"
	memberThingTypeName          = "ThingTypeName"
	memberThingGroupName         = "ThingGroupName"
	memberAttributePayload       = "AttributePayload"
	memberPrincipal              = "Principal"
	memberCertificateID          = "CertificateId"
	memberThingTypeProperties    = "ThingTypeProperties"
	memberThingGroupProperties   = "ThingGroupProperties"
	memberCertificatePem         = "CertificatePem"
	memberCertificateMode        = "CertificateMode"
	propertyDeprecate            = "DeprecateThingType"
	propertyCACertificatePem     = "CACertificatePem"
	propertyCSR                  = "CertificateSigningRequest"
	attributeID                  = "Id"
	iotNotFound                  = "ResourceNotFoundException"
	iotActive                    = "ACTIVE"
	iotInactive                  = "INACTIVE"
	memberRuleName               = "RuleName"
	memberTopicRulePayload       = "TopicRulePayload"
	memberAuthorizerName         = "AuthorizerName"
	memberAuthorizerFunctionArn  = "AuthorizerFunctionArn"
	memberTokenKeyName           = "TokenKeyName"
	memberTokenSigningPublicKeys = "TokenSigningPublicKeys"
	memberEnableCachingForHTTP   = "EnableCachingForHttp"
	propertySigningDisabled      = "SigningDisabled"
	memberRoleAlias              = "RoleAlias"
	memberSetAsDefault           = "SetAsDefault"
	memberTemplateName           = "TemplateName"
	memberTemplateArn            = "TemplateArn"
	memberTemplateBody           = "TemplateBody"
	memberProvisioningRoleArn    = "ProvisioningRoleArn"
	memberPreProvisioningHook    = "PreProvisioningHook"
	memberCredentialDuration     = "CredentialDurationSeconds" // #nosec G101 -- a property name, not a credential.
	// iotRuleNotFound is how IoT reports a missing topic rule.
	iotRuleNotFound = "UnauthorizedException"
)

// iotAttributes are the Arn and Id attributes of an IoT resource.
func iotAttributes(out map[string]any, arnMember, idMember string) map[string]any {
	return map[string]any{memberArn: outText(out, arnMember), attributeID: outText(out, idMember)}
}

// iotTagger tags an IoT resource by its Arn attribute.
func iotTagger(r *resource) *tagger { return iotTaggerAt(r.Attributes[memberArn]) }

// iotTaggerAt tags the IoT resource an ARN names.
func iotTaggerAt(resourceARN any) *tagger {
	return &tagger{
		target: map[string]any{memberResourceArn: resourceARN}, service: serviceIoT,
		tag: "TagResource", untag: "UntagResource", key: memberKey, value: memberValue,
	}
}

func init() {
	register("AWS::IoT::Thing", &resourceType{
		name:       memberThingName,
		properties: []string{memberThingName, memberAttributePayload},
		attributes: []string{memberArn, attributeID},
		replace:    []string{memberThingName},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			name := c.nameOf(p, memberThingName, 128)
			out, err := c.call(ctx, serviceIoT, "CreateThing",
				pick(map[string]any{memberThingName: name}, p, memberAttributePayload))
			if err != nil {
				return "", nil, err
			}

			return name, iotAttributes(out, "ThingArn", "ThingId"), nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			// The thing's attributes become the template's: a merge sets them
			// and removes, with "", those the template no longer has.
			attrs := maps.Clone(thingAttributes(p))
			for k := range thingAttributes(r.Properties) {
				if _, ok := attrs[k]; !ok {
					attrs[k] = ""
				}
			}

			_, err := c.call(ctx, serviceIoT, "UpdateThing", map[string]any{
				memberThingName:        r.Physical,
				memberAttributePayload: map[string]any{memberAttributes: attrs, "Merge": true},
			})
			return r.Attributes, err
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, serviceIoT, "DeleteThing", map[string]any{memberThingName: r.Physical})
			return err
		},
	})

	register("AWS::IoT::ThingType", &resourceType{
		name:       memberThingTypeName,
		properties: []string{memberThingTypeName, memberThingTypeProperties, propertyDeprecate, memberTags},
		attributes: []string{memberArn, attributeID},
		replace:    []string{memberThingTypeName, memberThingTypeProperties},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			name := c.nameOf(p, memberThingTypeName, 128)
			params := pick(map[string]any{memberThingTypeName: name}, p, memberThingTypeProperties)
			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagList(tags, memberKey, memberValue)
			}

			out, err := c.call(ctx, serviceIoT, "CreateThingType", params)
			if err != nil {
				return "", nil, err
			}

			if p.truth(propertyDeprecate) {
				if deprecateErr := deprecateThingType(ctx, c, name, true); deprecateErr != nil {
					return "", nil, deprecateErr
				}
			}

			return name, iotAttributes(out, "ThingTypeArn", "ThingTypeId"), nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if p.truth(propertyDeprecate) != props(r.Properties).truth(propertyDeprecate) {
				if err := deprecateThingType(ctx, c, r.Physical, p.truth(propertyDeprecate)); err != nil {
					return nil, err
				}
			}

			return r.Attributes, retag(ctx, c, r.Properties, p, iotTagger(r))
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			// A thing type is deprecated before it is deleted.
			err := deprecateThingType(ctx, c, r.Physical, true)
			if err == nil {
				_, err = c.call(ctx, serviceIoT, "DeleteThingType", map[string]any{memberThingTypeName: r.Physical})
			}

			if gone(err, iotNotFound) {
				return nil
			}

			return err
		},
	})

	register("AWS::IoT::ThingGroup", &resourceType{
		name:       memberThingGroupName,
		properties: []string{memberThingGroupName, "ParentGroupName", memberThingGroupProperties, memberTags},
		attributes: []string{memberArn, attributeID},
		replace:    []string{memberThingGroupName, "ParentGroupName"},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			name := c.nameOf(p, memberThingGroupName, 128)
			params := pick(map[string]any{memberThingGroupName: name}, p, "ParentGroupName", memberThingGroupProperties)
			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagList(tags, memberKey, memberValue)
			}

			out, err := c.call(ctx, serviceIoT, "CreateThingGroup", params)
			if err != nil {
				return "", nil, err
			}

			return name, iotAttributes(out, "ThingGroupArn", "ThingGroupId"), nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if p.changed(r.Properties, memberThingGroupProperties) {
				properties, _ := p[memberThingGroupProperties].(map[string]any)
				if properties == nil {
					properties = map[string]any{}
				}

				if _, err := c.call(ctx, serviceIoT, "UpdateThingGroup", map[string]any{
					memberThingGroupName: r.Physical, memberThingGroupProperties: properties,
				}); err != nil {
					return nil, err
				}
			}

			return r.Attributes, retag(ctx, c, r.Properties, p, iotTagger(r))
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, serviceIoT, "DeleteThingGroup", map[string]any{memberThingGroupName: r.Physical})
			return err
		},
	})

	register("AWS::IoT::Policy", &resourceType{
		name:       memberPolicyName,
		properties: []string{memberPolicyName, memberPolicyDocument, memberTags},
		attributes: []string{memberArn, attributeID},
		replace:    []string{memberPolicyName},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			name := c.nameOf(p, memberPolicyName, 128)
			params := map[string]any{memberPolicyName: name, memberPolicyDocument: p.document(memberPolicyDocument)}
			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagList(tags, memberKey, memberValue)
			}

			out, err := c.call(ctx, serviceIoT, "CreatePolicy", params)
			if err != nil {
				return "", nil, err
			}

			return name, map[string]any{memberArn: outText(out, memberPolicyArn), attributeID: name}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if p.changed(r.Properties, memberPolicyDocument) {
				if err := newPolicyVersion(ctx, c, r.Physical, p.document(memberPolicyDocument)); err != nil {
					return nil, err
				}
			}

			return r.Attributes, retag(ctx, c, r.Properties, p, iotTagger(r))
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			err := deletePolicyVersions(ctx, c, r.Physical, true)
			if err == nil {
				_, err = c.call(ctx, serviceIoT, "DeletePolicy", map[string]any{memberPolicyName: r.Physical})
			}

			if gone(err, iotNotFound) {
				return nil
			}

			return err
		},
	})

	register("AWS::IoT::TopicRule", &resourceType{
		name:       memberRuleName,
		properties: []string{memberRuleName, memberTopicRulePayload, memberTags},
		attributes: []string{memberArn},
		replace:    []string{memberRuleName},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			// Rule names take letters, digits, and underscores only.
			name := p.str(memberRuleName)
			if name == "" {
				name = strings.ReplaceAll(c.generate(128), "-", "_")
			}

			params := map[string]any{memberRuleName: name, memberTopicRulePayload: p[memberTopicRulePayload]}
			if tags := p.tags(); len(tags) > 0 {
				header := url.Values{}
				for k, v := range tags {
					header.Set(k, v)
				}

				params[memberTags] = header.Encode()
			}

			if _, err := c.call(ctx, serviceIoT, "CreateTopicRule", params); err != nil {
				return "", nil, err
			}

			arn := "arn:aws:iot:" + c.scope.Region + ":" + c.scope.AccountID + ":rule/" + name
			return name, map[string]any{memberArn: arn}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if p.changed(r.Properties, memberTopicRulePayload) {
				if _, err := c.call(ctx, serviceIoT, "ReplaceTopicRule", map[string]any{
					memberRuleName: r.Physical, memberTopicRulePayload: p[memberTopicRulePayload],
				}); err != nil {
					return nil, err
				}
			}

			return r.Attributes, retag(ctx, c, r.Properties, p, iotTagger(r))
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, serviceIoT, "DeleteTopicRule", map[string]any{memberRuleName: r.Physical})
			if gone(err, iotRuleNotFound) {
				return nil
			}

			return err
		},
	})

	register("AWS::IoT::Authorizer", &resourceType{
		name: memberAuthorizerName,
		properties: []string{
			memberAuthorizerName, memberAuthorizerFunctionArn, propertySigningDisabled, memberStatus,
			memberTokenKeyName, memberTokenSigningPublicKeys, memberEnableCachingForHTTP, memberTags,
		},
		attributes: []string{memberArn},
		replace:    []string{memberAuthorizerName, propertySigningDisabled},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			name := c.nameOf(p, memberAuthorizerName, 128)
			params := pick(map[string]any{memberAuthorizerName: name}, p, memberAuthorizerFunctionArn,
				propertySigningDisabled, memberStatus, memberTokenKeyName, memberTokenSigningPublicKeys,
				memberEnableCachingForHTTP)
			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagList(tags, memberKey, memberValue)
			}

			out, err := c.call(ctx, serviceIoT, "CreateAuthorizer", params)
			if err != nil {
				return "", nil, err
			}

			return name, map[string]any{memberArn: outText(out, "AuthorizerArn")}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := pick(map[string]any{memberAuthorizerName: r.Physical}, p, memberAuthorizerFunctionArn,
				memberStatus, memberTokenKeyName, memberTokenSigningPublicKeys, memberEnableCachingForHTTP)
			if _, err := c.call(ctx, serviceIoT, "UpdateAuthorizer", params); err != nil {
				return nil, err
			}

			return r.Attributes, retag(ctx, c, r.Properties, p, iotTagger(r))
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			// Only an inactive authorizer can be deleted.
			_, err := c.call(ctx, serviceIoT, "UpdateAuthorizer", map[string]any{
				memberAuthorizerName: r.Physical, memberStatus: iotInactive,
			})
			if err == nil {
				_, err = c.call(ctx, serviceIoT, "DeleteAuthorizer", map[string]any{memberAuthorizerName: r.Physical})
			}

			if gone(err, iotNotFound) {
				return nil
			}

			return err
		},
	})

	register("AWS::IoT::RoleAlias", &resourceType{
		name:       memberRoleAlias,
		properties: []string{memberRoleAlias, memberRoleArn, memberCredentialDuration, memberTags},
		attributes: []string{"RoleAliasArn"},
		replace:    []string{memberRoleAlias},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			name := c.nameOf(p, memberRoleAlias, 128)
			params := pick(map[string]any{memberRoleAlias: name}, p, memberRoleArn, memberCredentialDuration)
			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagList(tags, memberKey, memberValue)
			}

			out, err := c.call(ctx, serviceIoT, "CreateRoleAlias", params)
			if err != nil {
				return "", nil, err
			}

			return name, map[string]any{"RoleAliasArn": outText(out, "RoleAliasArn")}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if _, err := c.call(ctx, serviceIoT, "UpdateRoleAlias", pick(map[string]any{memberRoleAlias: r.Physical}, p,
				memberRoleArn, memberCredentialDuration)); err != nil {
				return nil, err
			}

			return r.Attributes, retag(ctx, c, r.Properties, p, iotTaggerAt(r.Attributes["RoleAliasArn"]))
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, serviceIoT, "DeleteRoleAlias", map[string]any{memberRoleAlias: r.Physical})
			if gone(err, iotNotFound) {
				return nil
			}

			return err
		},
	})

	register("AWS::IoT::ProvisioningTemplate", &resourceType{
		name: memberTemplateName,
		properties: []string{
			memberTemplateName, memberDescription, memberEnabled, memberProvisioningRoleArn, memberTemplateBody,
			"TemplateType", memberPreProvisioningHook, memberTags,
		},
		attributes: []string{memberTemplateArn},
		replace:    []string{memberTemplateName, "TemplateType"},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			name := strings.ReplaceAll(c.nameOf(p, memberTemplateName, 36), ".", "_")
			params := pick(map[string]any{memberTemplateName: name}, p, memberDescription, memberEnabled,
				memberProvisioningRoleArn, memberTemplateBody, memberPreProvisioningHook)
			if typ := p.str("TemplateType"); typ != "" {
				params["Type"] = typ
			}

			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagList(tags, memberKey, memberValue)
			}

			out, err := c.call(ctx, serviceIoT, "CreateProvisioningTemplate", params)
			if err != nil {
				return "", nil, err
			}

			return name, map[string]any{memberTemplateArn: outText(out, memberTemplateArn)}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if p.changed(r.Properties, memberTemplateBody) {
				if err := newTemplateVersion(ctx, c, r.Physical, p.str(memberTemplateBody)); err != nil {
					return nil, err
				}
			}

			params := pick(map[string]any{memberTemplateName: r.Physical}, p, memberDescription, memberEnabled,
				memberProvisioningRoleArn, memberPreProvisioningHook)
			if !p.has(memberPreProvisioningHook) && props(r.Properties).has(memberPreProvisioningHook) {
				params["RemovePreProvisioningHook"] = true
			}

			if _, err := c.call(ctx, serviceIoT, "UpdateProvisioningTemplate", params); err != nil {
				return nil, err
			}

			return r.Attributes, retag(ctx, c, r.Properties, p, iotTaggerAt(r.Attributes[memberTemplateArn]))
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(
				ctx,
				serviceIoT,
				"DeleteProvisioningTemplate",
				map[string]any{memberTemplateName: r.Physical},
			)
			if gone(err, iotNotFound) {
				return nil
			}

			return err
		},
	})

	register("AWS::IoT::PolicyPrincipalAttachment", &resourceType{
		properties: []string{memberPolicyName, memberPrincipal},
		attributes: []string{},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			if _, err := c.call(ctx, serviceIoT, "AttachPolicy", map[string]any{
				memberPolicyName: p.str(memberPolicyName), "Target": p.str(memberPrincipal),
			}); err != nil {
				return "", nil, err
			}

			return c.generate(128), map[string]any{}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, serviceIoT, "DetachPolicy", map[string]any{
				memberPolicyName: props(r.Properties).str(memberPolicyName),
				"Target":         props(r.Properties).str(memberPrincipal),
			})
			if gone(err, iotNotFound) {
				return nil
			}

			return err
		},
	})

	register("AWS::IoT::ThingPrincipalAttachment", &resourceType{
		properties: []string{memberThingName, memberPrincipal, "ThingPrincipalType"},
		attributes: []string{},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			if _, err := c.call(ctx, serviceIoT, "AttachThingPrincipal", pick(map[string]any{
				memberThingName: p.str(memberThingName), memberPrincipal: p.str(memberPrincipal),
			}, p, "ThingPrincipalType")); err != nil {
				return "", nil, err
			}

			return c.generate(128), map[string]any{}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, serviceIoT, "DetachThingPrincipal", map[string]any{
				memberThingName: props(r.Properties).str(memberThingName),
				memberPrincipal: props(r.Properties).str(memberPrincipal),
			})
			if gone(err, iotNotFound) {
				return nil
			}

			return err
		},
	})

	register("AWS::IoT::Certificate", &resourceType{
		properties: []string{
			memberStatus, propertyCSR, memberCertificatePem, propertyCACertificatePem, memberCertificateMode,
		},
		attributes: []string{memberArn, attributeID},
		replace:    []string{propertyCSR, memberCertificatePem, propertyCACertificatePem, memberCertificateMode},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			out, err := createCertificate(ctx, c, p)
			if err != nil {
				return "", nil, err
			}

			id := outText(out, memberCertificateID)
			return id, map[string]any{memberArn: outText(out, "CertificateArn"), attributeID: id}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			_, err := c.call(ctx, serviceIoT, "UpdateCertificate", map[string]any{
				memberCertificateID: r.Physical, "NewStatus": p.str(memberStatus),
			})
			return r.Attributes, err
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			// An active certificate is deactivated before it is deleted.
			_, err := c.call(ctx, serviceIoT, "UpdateCertificate", map[string]any{
				memberCertificateID: r.Physical, "NewStatus": iotInactive,
			})
			if err == nil || gone(err, "CertificateStateException") {
				_, err = c.call(ctx, serviceIoT, "DeleteCertificate", map[string]any{memberCertificateID: r.Physical})
			}

			if gone(err, iotNotFound) {
				return nil
			}

			return err
		},
	})
}

// thingAttributes returns the attributes of a thing's AttributePayload
// property, never nil.
func thingAttributes(p props) map[string]any {
	payload, _ := p[memberAttributePayload].(map[string]any)
	attrs, _ := payload[memberAttributes].(map[string]any)
	if attrs == nil {
		return map[string]any{}
	}

	return attrs
}

func deprecateThingType(ctx context.Context, c *client, name string, deprecate bool) error {
	_, err := c.call(ctx, serviceIoT, "DeprecateThingType", map[string]any{
		memberThingTypeName: name, "UndoDeprecate": !deprecate,
	})
	return err
}

// newPolicyVersion makes document a policy's default version, first deleting
// its oldest other version when it has the most a policy can keep.
func newPolicyVersion(ctx context.Context, c *client, name, document string) error {
	if err := deletePolicyVersions(ctx, c, name, false); err != nil {
		return err
	}

	_, err := c.call(ctx, serviceIoT, "CreatePolicyVersion", map[string]any{
		memberPolicyName: name, memberPolicyDocument: document, memberSetAsDefault: true,
	})
	return err
}

// deletePolicyVersions deletes a policy's versions other than the default:
// all of them, or only the oldest once the policy has five.
func deletePolicyVersions(ctx context.Context, c *client, name string, all bool) error {
	out, err := c.call(ctx, serviceIoT, "ListPolicyVersions", map[string]any{memberPolicyName: name})
	if err != nil {
		return err
	}

	versions, _ := out["PolicyVersions"].([]any)
	var others []string
	for _, v := range versions {
		m, _ := v.(map[string]any)
		if isDefault, _ := m["IsDefaultVersion"].(bool); !isDefault {
			others = append(others, outText(m, "VersionId"))
		}
	}

	if !all {
		const maxVersions = 5
		if len(versions) < maxVersions || len(others) == 0 {
			return nil
		}

		// Versions are listed newest first.
		others = others[len(others)-1:]
	}

	for _, id := range others {
		if _, err = c.call(ctx, serviceIoT, "DeletePolicyVersion", map[string]any{
			memberPolicyName: name, "PolicyVersionId": id,
		}); err != nil {
			return err
		}
	}

	return nil
}

// createCertificate signs a CSR or registers a certificate, as the template
// asks.
func createCertificate(ctx context.Context, c *client, p props) (map[string]any, error) {
	active := p.str(memberStatus) == iotActive
	switch {
	case p.str(propertyCSR) != "":
		return c.call(ctx, serviceIoT, "CreateCertificateFromCsr", map[string]any{
			propertyCSR: p.str(propertyCSR), "SetAsActive": active,
		})
	case p.str(memberCertificateMode) == "SNI_ONLY":
		return c.call(ctx, serviceIoT, "RegisterCertificateWithoutCA", map[string]any{
			memberCertificatePem: p.str(memberCertificatePem), memberStatus: p.str(memberStatus),
		})
	default:
		params := map[string]any{memberCertificatePem: p.str(memberCertificatePem), memberStatus: p.str(memberStatus)}
		if ca := p.str(propertyCACertificatePem); ca != "" {
			params["CaCertificatePem"] = ca
		}

		return c.call(ctx, serviceIoT, "RegisterCertificate", params)
	}
}

// newTemplateVersion makes a template body the template's default version,
// deleting the versions before it so the five-version limit never binds.
func newTemplateVersion(ctx context.Context, c *client, name, body string) error {
	out, err := c.call(ctx, serviceIoT, "ListProvisioningTemplateVersions", map[string]any{memberTemplateName: name})
	if err != nil {
		return err
	}

	if _, err = c.call(ctx, serviceIoT, "CreateProvisioningTemplateVersion", map[string]any{
		memberTemplateName: name, memberTemplateBody: body, memberSetAsDefault: true,
	}); err != nil {
		return err
	}

	versions, _ := out["Versions"].([]any)
	for _, v := range versions {
		version, _ := v.(map[string]any)
		if _, err = c.call(ctx, serviceIoT, "DeleteProvisioningTemplateVersion", map[string]any{
			memberTemplateName: name, "VersionId": version["VersionId"],
		}); err != nil {
			return err
		}
	}

	return nil
}

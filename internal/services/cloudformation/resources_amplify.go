package cloudformation

import (
	"context"
	"encoding/base64"
	"maps"
)

const (
	amplifyAutoSubdomain = "EnableAutoSubDomain"
	amplifyService       = "amplify"
	amplifyAppID         = "AppId"
	amplifyBranchName    = "BranchName"
	amplifySubPatterns   = "AutoSubDomainCreationPatterns"
	amplifySubRole       = "AutoSubDomainIAMRole"
	amplifyApp           = "App"
)

// Supported properties follow the AWS CloudFormation resource specification. AutoBranchCreationConfig
// is deliberately rejected here: its CF structure differs from the Hosting API's configuration structure.
func init() {
	register("AWS::Amplify::App", amplifyProvider(amplifyApp, amplifyApp, amplifyAppID, []string{
		memberName,
		memberDescription,
		"Repository",
		"Platform",
		"ComputeRoleArn",
		"IAMServiceRole",
		"OauthToken",
		"AccessToken",
		"EnvironmentVariables",
		"EnableBranchAutoDeletion",
		"BuildSpec",
		"CustomHeaders",
		"CustomRules",
		"BasicAuthConfig",
		"CacheConfig",
		"JobConfig",
		memberTags,
	}, nil, []string{amplifyAppID, "AppName", memberArn, "DefaultDomain"}))
	register("AWS::Amplify::Branch", amplifyProvider("Branch", "Branch", amplifyBranchName, []string{
		amplifyAppID,
		amplifyBranchName,
		memberDescription,
		"Stage",
		"Framework",
		"EnableAutoBuild",
		"EnablePerformanceMode",
		"EnablePullRequestPreview",
		"PullRequestEnvironmentName",
		"EnableSkewProtection",
		"Backend",
		"ComputeRoleArn",
		"EnvironmentVariables",
		"BuildSpec",
		"BasicAuthConfig",
		memberTags,
	}, []string{amplifyAppID, amplifyBranchName}, []string{memberArn, amplifyBranchName}))
	register(
		"AWS::Amplify::Domain",
		amplifyProvider("DomainAssociation", "DomainAssociation", memberDomainName, []string{
			amplifyAppID, memberDomainName, amplifyAutoSubdomain, "SubDomainSettings", amplifySubPatterns,
			amplifySubRole, "CertificateSettings",
		}, []string{amplifyAppID, memberDomainName}, []string{
			memberArn, memberDomainName, "DomainStatus", "StatusReason",
			amplifyAutoSubdomain, amplifySubPatterns, amplifySubRole, "CertificateRecord",
		}),
	)
}

func amplifyParams(p props) (map[string]any, error) {
	params := maps.Clone(map[string]any(p))
	delete(params, memberTags)
	for k, v := range params {
		if v == nil {
			return nil, validation("Amplify property %s cannot be null", k)
		}
	}

	if value, ok := params["EnvironmentVariables"]; ok {
		entries, valid := value.([]any)
		if !valid {
			return nil, validation("EnvironmentVariables must be a list")
		}

		env := map[string]any{}
		for _, entry := range entries {
			pair, valid := entry.(map[string]any)
			if !valid || len(pair) != 2 {
				return nil, validation("EnvironmentVariables requires Name and Value")
			}

			name, nok := pair[memberName].(string)
			val, vok := pair["Value"].(string)
			if !nok || !vok {
				return nil, validation("EnvironmentVariables requires string Name and Value")
			}

			if _, duplicate := env[name]; duplicate {
				return nil, validation("Duplicate environment variable")
			}

			env[name] = val
		}

		params["EnvironmentVariables"] = env
	}

	if value, ok := params["BasicAuthConfig"]; ok {
		auth, valid := value.(map[string]any)
		if !valid {
			return nil, validation("BasicAuthConfig must be an object")
		}

		for key := range auth {
			if key != "Username" && key != "Password" && key != "EnableBasicAuth" {
				return nil, validation("Unsupported BasicAuthConfig member %s", key)
			}
		}

		if enabled, ok := auth["EnableBasicAuth"]; ok {
			params["EnableBasicAuth"] = enabled
		}

		user, uok := auth["Username"].(string)
		password, pok := auth["Password"].(string)
		if uok != pok || auth["Username"] != nil && !uok || auth["Password"] != nil && !pok {
			return nil, validation("BasicAuthConfig requires string Username and Password together")
		}

		if uok {
			params["BasicAuthCredentials"] = base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
		}

		delete(params, "BasicAuthConfig")
	}

	if value, ok := params["CertificateSettings"]; ok {
		cert, valid := value.(map[string]any)
		if !valid {
			return nil, validation("CertificateSettings must be an object")
		}

		cert = maps.Clone(cert)
		for key := range cert {
			if key != "CertificateType" && key != "CustomCertificateArn" {
				return nil, validation("Unsupported CertificateSettings member %s", key)
			}
		}

		if kind, ok := cert["CertificateType"]; ok {
			cert["Type"] = kind
			delete(cert, "CertificateType")
		}

		params["CertificateSettings"] = cert
	}

	if role, ok := params["IAMServiceRole"]; ok {
		params["IamServiceRoleArn"] = role
		delete(params, "IAMServiceRole")
	}

	if p.has(memberTags) {
		params[memberTags] = tagMap(p.tags())
	}

	return params, nil
}

func amplifyAttributes(kind string, value map[string]any) map[string]any {
	switch kind {
	case amplifyApp:
		return map[string]any{
			amplifyAppID:    value[amplifyAppID],
			"AppName":       value[memberName],
			memberArn:       value["AppArn"],
			"DefaultDomain": value["DefaultDomain"],
		}

	case "Branch":
		return map[string]any{memberArn: value["BranchArn"], amplifyBranchName: value[amplifyBranchName]}
	default:
		return map[string]any{
			memberArn:            value["DomainAssociationArn"],
			memberDomainName:     value[memberDomainName],
			"DomainStatus":       value["DomainStatus"],
			"StatusReason":       value["StatusReason"],
			amplifyAutoSubdomain: value[amplifyAutoSubdomain],
			amplifySubPatterns:   value[amplifySubPatterns],
			amplifySubRole:       value[amplifySubRole],
			"CertificateRecord":  "",
		}
	}
}

func amplifyProvider(operation, output, idMember string, properties, replace, attributes []string) *resourceType {
	return &resourceType{
		properties: properties, replace: replace, attributes: attributes,
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params, err := amplifyParams(p)
			if err != nil {
				return "", nil, err
			}

			out, err := c.call(ctx, amplifyService, "Create"+operation, params)
			if err != nil {
				return "", nil, err
			}

			value, _ := out[output].(map[string]any)
			return outText(value, idMember), amplifyAttributes(output, value), nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			for key := range r.Properties {
				if key != memberTags && !p.has(key) {
					return nil, validation("Removing Amplify property %s is not supported", key)
				}
			}

			params, err := amplifyParams(p)
			if err != nil {
				return nil, err
			}

			delete(params, memberTags)
			if operation == amplifyApp {
				params[amplifyAppID] = r.Physical
			}

			out, err := c.call(ctx, amplifyService, "Update"+operation, params)
			if err != nil {
				return nil, err
			}

			value, _ := out[output].(map[string]any)
			attrs := amplifyAttributes(output, value)
			if operation != "DomainAssociation" {
				err = retag(ctx, c, r.Properties, p, &tagger{
					service: amplifyService, tag: opTagResource, untag: opUntagResource,
					target: map[string]any{memberResourceArn: attrs[memberArn]},
				})
				if err != nil {
					return nil, err
				}
			}

			return attrs, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			params := map[string]any{idMember: r.Physical}
			if operation == amplifyApp {
				params = map[string]any{amplifyAppID: r.Physical}
			} else {
				params[amplifyAppID] = props(r.Properties).str(amplifyAppID)
			}

			_, err := c.call(ctx, amplifyService, "Delete"+operation, params)
			if gone(err, "NotFoundException") {
				return nil
			}

			return err
		},
	}
}

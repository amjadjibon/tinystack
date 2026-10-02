package cloudformation

import (
	"archive/zip"
	"bytes"
	"context"
	"strings"
)

// functionSettings are the AWS::Lambda::Function properties
// UpdateFunctionConfiguration changes.
var functionSettings = []string{
	propertyRole,
	"Handler",
	memberDescription,
	"Timeout",
	"MemorySize",
	propertyEnvironment,
	"Runtime",
}

func init() {
	register("AWS::Lambda::Function", &resourceType{
		name: memberFunctionName,
		properties: append([]string{memberFunctionName, "Code", propertyPackageType, "Architectures", memberTags},
			functionSettings...),
		attributes: []string{memberArn},
		replace:    []string{memberFunctionName, propertyPackageType},
		create:     createFunction,
		update:     updateFunction,
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "lambda", "DeleteFunction", map[string]any{memberFunctionName: r.Physical})
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
	register("AWS::Lambda::Permission", &resourceType{
		properties: []string{
			memberFunctionName,
			propertyAction,
			propertyPrincipal,
			"SourceArn",
			"SourceAccount",
			"PrincipalOrgID",
			"EventSourceToken",
		},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			id := c.generate(100)
			params := pick(
				map[string]any{"StatementId": id},
				p,
				memberFunctionName,
				propertyAction,
				propertyPrincipal,
				"SourceArn",
				"SourceAccount",
				"PrincipalOrgID",
				"EventSourceToken",
			)
			if _, err := c.call(ctx, "lambda", "AddPermission", params); err != nil {
				return "", nil, err
			}

			return id, nil, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "lambda", "RemovePermission", map[string]any{
				memberFunctionName: props(r.Properties).str(memberFunctionName), "StatementId": r.Physical,
			})
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
	register("AWS::Lambda::EventSourceMapping", &resourceType{
		properties: []string{
			"EventSourceArn",
			memberFunctionName,
			propertyBatchSize,
			memberEnabled,
			"StartingPosition",
			propertyMaximumBatchingWindow,
			"MaximumRetryAttempts",
			propertyFilterCriteria,
			propertyFunctionResponseTypes,
		},
		attributes: []string{"Id", "EventSourceMappingArn"},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			out, err := c.call(ctx, "lambda", "CreateEventSourceMapping", pick(
				map[string]any{},
				p,
				"EventSourceArn",
				memberFunctionName,
				propertyBatchSize,
				memberEnabled,
				"StartingPosition",
				propertyMaximumBatchingWindow,
				"MaximumRetryAttempts",
				propertyFilterCriteria,
				propertyFunctionResponseTypes,
			))
			if err != nil {
				return "", nil, err
			}

			id := outText(out, "UUID")
			arn := "arn:aws:lambda:" + c.scope.Region + ":" + c.scope.AccountID + ":event-source-mapping:" + id
			return id, map[string]any{"Id": id, "EventSourceMappingArn": arn}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "lambda", "DeleteEventSourceMapping", map[string]any{"UUID": r.Physical})
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
	register("AWS::Lambda::Version", &resourceType{
		properties: []string{memberFunctionName, memberDescription, "CodeSha256"},
		attributes: []string{memberVersion, "FunctionArn"},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			out, err := c.call(ctx, "lambda", "PublishVersion",
				pick(map[string]any{}, p, memberFunctionName, memberDescription, "CodeSha256"))
			if err != nil {
				return "", nil, err
			}

			arn := outText(out, "FunctionArn")
			return arn, map[string]any{memberVersion: outText(out, memberVersion), "FunctionArn": arn}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "lambda", "DeleteFunction", map[string]any{
				memberFunctionName: props(r.Properties).str(memberFunctionName),
				"Qualifier":        outText(r.Attributes, memberVersion),
			})
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
	register("AWS::Lambda::Alias", &resourceType{
		properties: []string{
			memberFunctionName,
			memberName,
			memberFunctionVersion,
			memberDescription,
			memberRoutingConfig,
		},
		attributes: []string{"AliasArn"},
		replace:    []string{memberFunctionName, memberName},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params, err := aliasInput(p)
			if err != nil {
				return "", nil, err
			}

			out, err := c.call(ctx, "lambda", "CreateAlias", params)
			if err != nil {
				return "", nil, err
			}

			arn := outText(out, "AliasArn")
			return arn, map[string]any{"AliasArn": arn}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params, err := aliasInput(p)
			if err != nil {
				return nil, err
			}

			if !p.has(memberDescription) {
				params[memberDescription] = ""
			}

			if !p.has(memberRoutingConfig) {
				params[memberRoutingConfig] = map[string]any{memberAdditionalVersionWeights: map[string]any{}}
			}

			_, err = c.call(ctx, "lambda", "UpdateAlias", params)
			return r.Attributes, err
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			p := props(r.Properties)
			_, err := c.call(ctx, "lambda", "DeleteAlias", map[string]any{
				memberFunctionName: p.str(memberFunctionName), memberName: p.str(memberName),
			})
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
}

// aliasInput renders an alias's properties as CreateAlias's input:
// RoutingConfig's list of version weights becomes Lambda's map.
func aliasInput(p props) (map[string]any, error) {
	params := pick(map[string]any{}, p, memberFunctionName, memberName, memberFunctionVersion, memberDescription)
	routing, ok := p[memberRoutingConfig].(map[string]any)
	if !ok {
		return params, nil
	}

	weights := map[string]any{}
	for k, v := range routing {
		if k != memberAdditionalVersionWeights {
			return nil, validation("TinyStack does not support the %s property of RoutingConfig", k)
		}

		list, _ := v.([]any)
		for _, el := range list {
			m, _ := el.(map[string]any)
			w := props(m)
			weights[w.str(memberFunctionVersion)] = w["FunctionWeight"]
		}
	}

	params[memberRoutingConfig] = map[string]any{memberAdditionalVersionWeights: weights}
	return params, nil
}

// code renders a function's Code property as Lambda's: inline ZipFile source
// becomes a zip archive holding index.js or index.py, as CloudFormation
// writes it.
func code(p props) (map[string]any, error) {
	spec, _ := p["Code"].(map[string]any)
	source, inline := spec["ZipFile"]
	if !inline {
		return pick(map[string]any{}, spec, "S3Bucket", "S3Key", "S3ObjectVersion", "ImageUri"), nil
	}

	runtime := p.str("Runtime")
	var file string
	switch {
	case strings.HasPrefix(runtime, "nodejs"):
		file = "index.js"
	case strings.HasPrefix(runtime, "python"):
		file = "index.py"
	default:
		return nil, validation("ZipFile code is only supported for Node.js and Python runtimes, not %q", runtime)
	}

	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, err := w.Create(file)
	if err != nil {
		return nil, err
	}

	if _, err = f.Write([]byte(scalarText(source))); err != nil {
		return nil, err
	}

	err = w.Close()
	if err != nil {
		return nil, err
	}

	return map[string]any{"ZipFile": buf.String()}, nil
}

func createFunction(ctx context.Context, c *client, p props) (physical string, attributes map[string]any, err error) {
	name := c.nameOf(p, memberFunctionName, 64)
	source, err := code(p)
	if err != nil {
		return "", nil, err
	}

	params := pick(map[string]any{memberFunctionName: name, "Code": source}, p, functionSettings...)
	pick(params, p, propertyPackageType, "Architectures")
	if tags := p.tags(); len(tags) > 0 {
		params[memberTags] = tagMap(tags)
	}

	out, err := c.call(ctx, "lambda", "CreateFunction", params)
	if err != nil {
		return "", nil, err
	}

	return name, map[string]any{memberArn: outText(out, "FunctionArn")}, nil
}

func updateFunction(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
	old := props(r.Properties)
	settings := map[string]any{memberFunctionName: r.Physical}
	changed := false
	for _, k := range functionSettings {
		if !p.changed(old, k) {
			continue
		}

		changed = true
		switch {
		case p.has(k):
			settings[k] = p[k]
		case k == propertyEnvironment:
			settings[k] = map[string]any{memberVariables: map[string]any{}}
		case k == memberDescription:
			settings[k] = ""
		}
	}

	if changed {
		if _, err := c.call(ctx, "lambda", "UpdateFunctionConfiguration", settings); err != nil {
			return nil, err
		}
	}

	if p.changed(old, "Code") || p.changed(old, "Architectures") || p.changed(old, "Runtime") {
		source, err := code(p)
		if err != nil {
			return nil, err
		}

		params := pick(map[string]any{memberFunctionName: r.Physical}, p, "Architectures")
		for k, v := range source {
			params[k] = v
		}

		if _, err = c.call(ctx, "lambda", "UpdateFunctionCode", params); err != nil {
			return nil, err
		}
	}

	return r.Attributes, retag(ctx, c, old, p, &tagger{
		target:  map[string]any{"Resource": outText(r.Attributes, memberArn)},
		service: "lambda", tag: opTagResource, untag: opUntagResource,
	})
}

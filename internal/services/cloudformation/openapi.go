package cloudformation

import (
	"context"
	"encoding/json"
	"strings"

	"gopkg.in/yaml.v3"
)

const apiBody = "Body"

const apiImportMode = "Mode"

const apiFailOnWarnings = "FailOnWarnings"

// apiDefinition reads S3 through the catalog, after resource intrinsics have resolved.
func apiDefinition(ctx context.Context, c *client, p props) (string, error) {
	var root map[string]any
	if p.has("BodyS3Location") {
		location, _ := p["BodyS3Location"].(map[string]any)
		if location == nil {
			return "", validation("BodyS3Location must contain Bucket and Key")
		}

		params := map[string]any{memberBucket: location[memberBucket], memberKey: location[memberKey]}
		if v := location["Version"]; v != nil {
			params["VersionId"] = v
		}

		out, err := c.call(ctx, "s3", "GetObject", params)
		if err != nil {
			return "", err
		}

		if err = yaml.Unmarshal([]byte(outText(out, apiBody)), &root); err != nil {
			return "", validation("Invalid S3 API definition: %s", err)
		}
	}

	if p.has(apiBody) {
		var body map[string]any
		switch v := p[apiBody].(type) {
		case string:
			if err := yaml.Unmarshal([]byte(v), &body); err != nil {
				return "", validation("Invalid API Body: %s", err)
			}
		case map[string]any:
			body = v
		default:
			return "", validation("API Body must be an object or string")
		}

		if root == nil {
			root = body
		} else {
			// SAM adds implicit event operations only when the definition lacks them.
			paths, _ := root["paths"].(map[string]any)
			if paths == nil {
				paths = map[string]any{}
				root["paths"] = paths
			}

			overlay, _ := body["paths"].(map[string]any)
			for path, raw := range overlay {
				existing, _ := paths[path].(map[string]any)
				if existing == nil {
					paths[path] = raw
					continue
				}

				methods, _ := raw.(map[string]any)
				for method, op := range methods {
					if _, ok := existing[method]; !ok {
						existing[method] = op
					}
				}
			}
		}
	}

	if root == nil {
		return "", validation("API definition is empty")
	}

	b, err := json.Marshal(root)
	return string(b), err
}

func samAPIDefinition(target map[string]any, p props) {
	if p.has("DefinitionBody") {
		target[apiBody] = p["DefinitionBody"]
	}

	if value, ok := p["DefinitionUri"]; ok {
		switch uri := value.(type) {
		case string:
			bucket, key, _ := strings.Cut(strings.TrimPrefix(uri, "s3://"), "/")
			target["BodyS3Location"] = map[string]any{memberBucket: bucket, memberKey: key}
		default:
			target["BodyS3Location"] = uri
		}
	}
}

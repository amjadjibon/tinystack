package cloudformation

import (
	"context"
	"fmt"
	"reflect"
)

const mlEncryptionKey = "EncryptionKey"

func init() { registerMLTranslate(false); registerMLTranslate(true) }

func mlTagger(service, arn string) *tagger {
	return &tagger{
		service: service,
		tag:     opTagResource,
		untag:   opUntagResource,
		target:  map[string]any{memberResourceArn: arn},
		key:     memberKey,
		value:   memberValue,
	}
}

func registerMLTranslate(parallel bool) {
	suffix, kind, createOp, config := "Terminology", "terminology", "ImportTerminology", "TerminologyData"
	immutable := []string{memberName, mlEncryptionKey}
	properties := []string{
		memberName,
		memberDescription,
		mlEncryptionKey,
		memberTags,
		"TerminologyData",
		"MergeStrategy",
	}
	if parallel {
		suffix, kind, createOp, config = "ParallelData", "parallel-data", "CreateParallelData", "ParallelDataConfig"
		immutable = append(immutable, memberDescription)
		properties = []string{memberName, memberDescription, mlEncryptionKey, memberTags, config}
	}

	attributes := func(c *client, id string) map[string]any {
		if !parallel {
			id += "/LATEST"
		}

		return map[string]any{
			memberArn: fmt.Sprintf("arn:aws:translate:%s:%s:%s/%s", c.scope.Region, c.scope.AccountID, kind, id),
		}
	}
	params := func(p props) map[string]any {
		out := pick(map[string]any{}, p, memberName, memberDescription, mlEncryptionKey, config)
		if !parallel {
			out["MergeStrategy"] = "OVERWRITE"
			if p.has("MergeStrategy") {
				out["MergeStrategy"] = p["MergeStrategy"]
			}
		}

		return out
	}
	register("AWS::Translate::"+suffix, &resourceType{
		name: memberName, properties: properties, attributes: []string{memberArn}, replace: immutable,
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			in := params(p)
			if parallel {
				in["ClientToken"] = c.generate(64)
			}

			in[memberTags] = tagList(p.tags(), memberKey, memberValue)
			if _, err := c.call(ctx, "translate", createOp, in); err != nil {
				return "", nil, err
			}

			id := p.str(memberName)
			return id, attributes(c, id), nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			in := params(p)
			in[memberName] = r.Physical
			op := createOp
			if parallel {
				delete(in, mlEncryptionKey)
				op = "UpdateParallelData"
				in["ClientToken"] = c.generate(64)
			}

			changed := !reflect.DeepEqual(r.Properties[config], p[config]) ||
				!reflect.DeepEqual(r.Properties[memberDescription], p[memberDescription]) ||
				!reflect.DeepEqual(r.Properties["MergeStrategy"], p["MergeStrategy"])
			if changed {
				if !p.has(memberDescription) {
					in[memberDescription] = ""
				}

				if _, err := c.call(ctx, "translate", op, in); err != nil {
					return nil, err
				}
			}

			attrs := attributes(c, r.Physical)
			err := retag(ctx, c, r.Properties, p, mlTagger("translate", outText(attrs, memberArn)))
			return attrs, err
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "translate", "Delete"+suffix, map[string]any{memberName: r.Physical})
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
}

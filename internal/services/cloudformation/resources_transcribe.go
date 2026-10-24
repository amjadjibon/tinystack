package cloudformation

import (
	"context"
	"fmt"
	"reflect"
)

func init() { registerMLVocabulary(false); registerMLVocabulary(true) }

func registerMLVocabulary(filter bool) {
	suffix, kind, name, content, uri := "Vocabulary", "vocabulary", "VocabularyName", "Phrases", "VocabularyFileUri"
	if filter {
		suffix, kind = "VocabularyFilter", "vocabulary-filter"
		name, content, uri = "VocabularyFilterName", "Words", "VocabularyFilterFileUri"
	}

	attributes := func(c *client, id string) map[string]any {
		return map[string]any{
			memberArn: fmt.Sprintf("arn:aws:transcribe:%s:%s:%s/%s", c.scope.Region, c.scope.AccountID, kind, id),
		}
	}
	register("AWS::Transcribe::"+suffix, &resourceType{
		name:       name,
		properties: []string{name, "LanguageCode", content, uri, memberTags},
		attributes: []string{memberArn},
		replace:    []string{name, "LanguageCode"},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := pick(map[string]any{}, p, name, "LanguageCode", content, uri)
			params[memberTags] = tagList(p.tags(), memberKey, memberValue)
			out, err := c.call(ctx, "transcribe", "Create"+suffix, params)
			if err != nil {
				return "", nil, err
			}

			id := outText(out, name)
			return id, attributes(c, id), nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := pick(map[string]any{name: r.Physical}, p, content, uri)
			if !filter {
				params["LanguageCode"] = p["LanguageCode"]
			}

			if !reflect.DeepEqual(r.Properties[content], p[content]) || !reflect.DeepEqual(r.Properties[uri], p[uri]) {
				if _, err := c.call(ctx, "transcribe", "Update"+suffix, params); err != nil {
					return nil, err
				}
			}

			attrs := attributes(c, r.Physical)
			err := retag(ctx, c, r.Properties, p, mlTagger("transcribe", outText(attrs, memberArn)))
			return attrs, err
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "transcribe", "Delete"+suffix, map[string]any{name: r.Physical})
			if gone(err, "NotFoundException") {
				return nil
			}

			return err
		},
	})
}

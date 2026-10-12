package cloudformation

import (
	"context"
)

const (
	codecommitRepositoryType = "AWS::CodeCommit::Repository"
	serviceCodeCommit        = "codecommit"
	propertyRepoName         = "RepositoryName"
	propertyRepoDesc         = "RepositoryDescription"
	propertyKmsKeyID         = "KmsKeyId"
	propertyRepoCode         = "Code"
	propertyRepoTriggers     = "Triggers"
	propertyCloneURLHTTP     = "CloneUrlHttp"
	propertyCloneURLSSH      = "CloneUrlSsh"
)

func init() {
	register(codecommitRepositoryType, &resourceType{
		properties: []string{
			propertyRepoName,
			propertyRepoDesc,
			propertyKmsKeyID,
			memberTags,
			propertyRepoCode,
			propertyRepoTriggers,
		},
		attributes: []string{
			memberArn,
			propertyCloneURLHTTP,
			propertyCloneURLSSH,
			memberName,
		},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			if p.has(propertyRepoCode) {
				return "", nil, validation("CodeCommit property Code is not supported")
			}

			if p.has(propertyRepoTriggers) {
				return "", nil, validation("CodeCommit property Triggers is not supported")
			}

			name := c.nameOf(p, propertyRepoName, 100)
			params := map[string]any{
				propertyRepoName: name,
			}

			if p.has(propertyRepoDesc) {
				params[propertyRepoDesc] = p[propertyRepoDesc]
			}

			if p.has(propertyKmsKeyID) {
				params[propertyKmsKeyID] = p[propertyKmsKeyID]
			}

			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagMap(tags)
			}

			out, err := c.call(ctx, serviceCodeCommit, "CreateRepository", params)
			if err != nil {
				return "", nil, err
			}

			meta, _ := out["RepositoryMetadata"].(map[string]any)
			repoID := outText(meta, "RepositoryId")
			attrs := map[string]any{
				memberArn:            meta["Arn"],
				propertyCloneURLHTTP: meta[propertyCloneURLHTTP],
				propertyCloneURLSSH:  meta[propertyCloneURLSSH],
				memberName:           meta["RepositoryName"],
			}

			return repoID, attrs, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if p.has(propertyRepoCode) {
				return nil, validation("CodeCommit property Code is not supported")
			}

			if p.has(propertyRepoTriggers) {
				return nil, validation("CodeCommit property Triggers is not supported")
			}

			// TinyStack does not implement UpdateRepositoryEncryptionKey, so a key change cannot be applied.
			if p.changed(props(r.Properties), propertyKmsKeyID) {
				return nil, validation("Changing the KmsKeyId of a CodeCommit repository is not supported")
			}

			currName := outText(r.Attributes, memberName)
			if currName == "" {
				currName = props(r.Properties).str(propertyRepoName)
			}

			newName := p.str(propertyRepoName)
			if newName != "" && newName != currName {
				_, err := c.call(ctx, serviceCodeCommit, "UpdateRepositoryName", map[string]any{
					"OldName": currName,
					"NewName": newName,
				})
				if err != nil {
					return nil, err
				}

				currName = newName
			}

			if p.changed(props(r.Properties), propertyRepoDesc) {
				_, err := c.call(ctx, serviceCodeCommit, "UpdateRepositoryDescription", map[string]any{
					propertyRepoName: currName,
					propertyRepoDesc: p.str(propertyRepoDesc),
				})
				if err != nil {
					return nil, err
				}
			}

			out, err := c.call(ctx, serviceCodeCommit, "GetRepository", map[string]any{
				propertyRepoName: currName,
			})
			if err != nil {
				return nil, err
			}

			meta, _ := out["RepositoryMetadata"].(map[string]any)
			repoArn := outText(meta, "Arn")

			err = retag(ctx, c, props(r.Properties), p, &tagger{
				target:  map[string]any{"ResourceArn": repoArn},
				service: serviceCodeCommit,
				tag:     opTagResource,
				untag:   opUntagResource,
			})
			if err != nil {
				return nil, err
			}

			attrs := map[string]any{
				memberArn:            meta["Arn"],
				propertyCloneURLHTTP: meta[propertyCloneURLHTTP],
				propertyCloneURLSSH:  meta[propertyCloneURLSSH],
				memberName:           meta["RepositoryName"],
			}

			return attrs, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			name := outText(r.Attributes, memberName)
			if name == "" {
				name = props(r.Properties).str(propertyRepoName)
			}

			if name == "" {
				return nil
			}

			_, err := c.call(ctx, serviceCodeCommit, "DeleteRepository", map[string]any{
				propertyRepoName: name,
			})
			return err
		},
	})
}

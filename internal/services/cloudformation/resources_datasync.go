package cloudformation

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/datasync"
	"github.com/amjadjibon/tinystack/internal/aws/request"
)

const (
	dataSyncTaskSuffix  = "Task"
	dataSyncLocationArn = "LocationArn"
	dataSyncLocationURI = "LocationUri"
	dataSyncService     = "datasync"
	dataSyncS3Type      = "AWS::DataSync::LocationS3"
	dataSyncEfsType     = "AWS::DataSync::LocationEFS"
	dataSyncTaskType    = "AWS::DataSync::Task"
)

func dataSyncRetag(ctx context.Context, c *client, r *resource, p props) error {
	set, removed := tagChanges(props(r.Properties).tags(), p.tags())
	if len(removed) > 0 {
		if _, err := c.call(
			ctx,
			dataSyncService,
			opUntagResource,
			map[string]any{memberResourceArn: r.Physical, "Keys": removed},
		); err != nil {
			return err
		}
	}

	if len(set) > 0 {
		_, err := c.call(
			ctx,
			dataSyncService,
			opTagResource,
			map[string]any{memberResourceArn: r.Physical, memberTags: tagList(set, memberKey, memberValue)},
		)
		return err
	}

	return nil
}

func init() {
	for _, definition := range []struct {
		typ     string
		suffix  string
		id      string
		fields  []string
		replace []string
	}{
		{
			typ: dataSyncS3Type, suffix: "LocationS3", id: dataSyncLocationArn, fields: []string{
				"S3BucketArn", "S3Config", "S3StorageClass", "Subdirectory", memberTags,
			},
			replace: []string{"S3BucketArn"},
		},
		{
			typ: dataSyncEfsType, suffix: "LocationEfs", id: dataSyncLocationArn, fields: []string{
				"EfsFilesystemArn",
				"Ec2Config",
				"Subdirectory",
				"AccessPointArn",
				"FileSystemAccessRoleArn",
				"InTransitEncryption",
				memberTags,
			},
			replace: []string{"EfsFilesystemArn", "Ec2Config"},
		},
		{
			typ: dataSyncTaskType, suffix: dataSyncTaskSuffix, id: "TaskArn", fields: []string{
				"SourceLocationArn",
				"DestinationLocationArn",
				memberName,
				"Options",
				"Includes",
				"Excludes",
				"Schedule",
				"CloudWatchLogGroupArn",
				"TaskMode",
				memberTags,
			},
			replace: []string{"SourceLocationArn", "DestinationLocationArn", "TaskMode"},
		},
	} {
		attributes := []string{definition.id}
		if definition.id == dataSyncLocationArn {
			attributes = append(attributes, dataSyncLocationURI)
		}

		register(definition.typ, &resourceType{
			properties: definition.fields,
			attributes: attributes,
			replace:    definition.replace,
			create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
				params := pick(map[string]any{}, p, definition.fields...)
				out, err := c.call(ctx, dataSyncService, "Create"+definition.suffix, params)
				if err != nil {
					return "", nil, err
				}

				arn := outText(out, definition.id)
				attrs := map[string]any{definition.id: arn}
				if definition.id == dataSyncLocationArn {
					described, e := c.call(
						ctx,
						dataSyncService,
						"Describe"+definition.suffix,
						map[string]any{definition.id: arn},
					)
					if e != nil {
						return "", nil, e
					}

					attrs[dataSyncLocationURI] = described[dataSyncLocationURI]
				}

				return arn, attrs, nil
			},
			update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
				params := pick(map[string]any{}, p, definition.fields...)
				delete(params, memberTags)
				for _, key := range definition.replace {
					delete(params, key)
				}

				params[definition.id] = r.Physical
				if definition.suffix == "LocationS3" {
					if !p.has("Subdirectory") {
						params["Subdirectory"] = ""
					}

					if !p.has("S3StorageClass") {
						params["S3StorageClass"] = "STANDARD"
					}
				}

				if definition.suffix == "LocationEfs" && !p.has("Subdirectory") {
					params["Subdirectory"] = ""
				}

				if definition.suffix == dataSyncTaskSuffix {
					if !p.has("Includes") {
						params["Includes"] = []any{}
					}

					if !p.has("Excludes") {
						params["Excludes"] = []any{}
					}
				}

				if err := c.dataSyncUpdate(ctx, "Update"+definition.suffix, params); err != nil {
					return nil, err
				}

				if err := dataSyncRetag(ctx, c, r, p); err != nil {
					return nil, err
				}

				if definition.id == dataSyncLocationArn {
					described, err := c.call(
						ctx,
						dataSyncService,
						"Describe"+definition.suffix,
						map[string]any{definition.id: r.Physical},
					)
					if err != nil {
						return nil, err
					}

					return map[string]any{
						definition.id:       r.Physical,
						dataSyncLocationURI: described[dataSyncLocationURI],
					}, nil
				}

				return r.Attributes, nil
			},
			remove: func(ctx context.Context, c *client, r *resource) error {
				op := "DeleteLocation"
				if definition.suffix == dataSyncTaskSuffix {
					op = "DeleteTask"
				}

				_, err := c.call(ctx, dataSyncService, op, map[string]any{definition.id: r.Physical})
				return err
			},
		})
	}
}

type dataSyncRecords interface {
	CloudFormationUpdateTask(context.Context, *api.UpdateTaskRequest) (*api.UpdateTaskResponse, error)
	CloudFormationUpdateLocationEfs(
		context.Context,
		*api.UpdateLocationEfsRequest,
	) (*api.UpdateLocationEfsResponse, error)
}

// UseDataSync connects complete template updates, including property removal.
func (s *Service) UseDataSync(records dataSyncRecords) { s.dataSync = records }

func (c *client) dataSyncUpdate(ctx context.Context, operation string, params map[string]any) error {
	if operation == "UpdateLocationS3" {
		_, err := c.call(ctx, dataSyncService, operation, params)
		return err
	}

	if c.dataSync == nil {
		return validation("DataSync template updates are not connected")
	}

	_, input, err := c.caller.Decode(dataSyncService, operation, params)
	if err != nil {
		return err
	}

	ctx = request.WithMetadata(ctx, request.Metadata{Scope: c.scope, Service: dataSyncService, Endpoint: c.endpoint})
	switch in := input.(type) {
	case *api.UpdateTaskRequest:
		_, err = c.dataSync.CloudFormationUpdateTask(ctx, in)
	case *api.UpdateLocationEfsRequest:
		_, err = c.dataSync.CloudFormationUpdateLocationEfs(ctx, in)
	}

	return err
}

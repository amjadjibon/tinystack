package cloudformation

import "context"

const (
	timestreamQueryService          = "timestreamquery"
	timestreamScheduledQueryName    = "ScheduledQueryName"
	timestreamScheduledQueryARN     = "ScheduledQueryArn"
	timestreamQueryString           = "QueryString"
	timestreamScheduleConfig        = "ScheduleConfiguration"
	timestreamNotificationConfig    = "NotificationConfiguration"
	timestreamExecutionRoleARN      = "ScheduledQueryExecutionRoleArn"
	timestreamTargetConfig          = "TargetConfiguration"
	timestreamErrorReportConfig     = "ErrorReportConfiguration"
	timestreamScheduledQueryToken   = "ClientToken"
	attrSQName                      = "SQName"
	attrSQQueryString               = "SQQueryString"
	attrSQScheduleConfiguration     = "SQScheduleConfiguration"
	attrSQNotificationConfiguration = "SQNotificationConfiguration"
	attrSQExecutionRoleARN          = "SQScheduledQueryExecutionRoleArn"
	attrSQTargetConfiguration       = "SQTargetConfiguration"
	attrSQErrorReportConfiguration  = "SQErrorReportConfiguration"
	attrSQKmsKeyID                  = "SQKmsKeyId"
)

func init() {
	immutable := []string{
		timestreamScheduledQueryToken,
		timestreamErrorReportConfig,
		memberKmsKeyID,
		timestreamNotificationConfig,
		timestreamQueryString,
		timestreamScheduleConfig,
		timestreamExecutionRoleARN,
		timestreamScheduledQueryName,
		timestreamTargetConfig,
	}
	register("AWS::Timestream::ScheduledQuery", &resourceType{
		name:       timestreamScheduledQueryName,
		properties: append(append([]string{}, immutable...), memberTags),
		attributes: []string{
			memberArn,
			attrSQErrorReportConfiguration,
			attrSQKmsKeyID,
			attrSQName,
			attrSQNotificationConfiguration,
			attrSQQueryString,
			attrSQScheduleConfiguration,
			attrSQExecutionRoleARN,
			attrSQTargetConfiguration,
		},
		replace: immutable,
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			name := c.nameOf(p, timestreamScheduledQueryName, 64)
			params := pick(
				map[string]any{memberName: name},
				p,
				timestreamScheduledQueryToken,
				timestreamErrorReportConfig,
				memberKmsKeyID,
				timestreamNotificationConfig,
				timestreamQueryString,
				timestreamScheduleConfig,
				timestreamExecutionRoleARN,
				timestreamTargetConfig,
			)
			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagList(tags, memberKey, memberValue)
			}

			out, err := c.call(ctx, timestreamQueryService, "CreateScheduledQuery", params)
			if err != nil {
				return "", nil, err
			}

			arn := outText(out, memberArn)
			return arn, map[string]any{
				memberArn:                       arn,
				attrSQErrorReportConfiguration:  p.attribute(timestreamErrorReportConfig),
				attrSQKmsKeyID:                  p.str(memberKmsKeyID),
				attrSQName:                      name,
				attrSQNotificationConfiguration: p.attribute(timestreamNotificationConfig),
				attrSQQueryString:               p.str(timestreamQueryString),
				attrSQScheduleConfiguration:     p.attribute(timestreamScheduleConfig),
				attrSQExecutionRoleARN:          p.str(timestreamExecutionRoleARN),
				attrSQTargetConfiguration:       p.attribute(timestreamTargetConfig),
			}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			err := retag(ctx, c, r.Properties, p, &tagger{
				target:  map[string]any{memberResourceARN: r.Physical},
				service: timestreamQueryService,
				tag:     opTagResource,
				untag:   opUntagResource,
				key:     memberKey,
				value:   memberValue,
			})
			return r.Attributes, err
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, timestreamQueryService, "DeleteScheduledQuery", map[string]any{
				timestreamScheduledQueryARN: r.Physical,
			})
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
}

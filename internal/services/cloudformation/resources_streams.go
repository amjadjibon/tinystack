package cloudformation

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// alarmProperties are the AWS::CloudWatch::Alarm properties, which
// PutMetricAlarm takes by the same names.
var alarmProperties = []string{
	"AlarmDescription", "ActionsEnabled", "OKActions", "AlarmActions", "InsufficientDataActions", "MetricName",
	"Namespace", "Statistic", "ExtendedStatistic", "Dimensions", "Period", "Unit", "EvaluationPeriods",
	"DatapointsToAlarm", "Threshold", "ComparisonOperator", "TreatMissingData", "EvaluateLowSampleCountPercentile",
	"Metrics", "ThresholdMetricId",
}

// defaultRetention is a Kinesis stream's retention period in hours unless
// it sets one.
const defaultRetention = 24

func init() {
	register("AWS::StepFunctions::StateMachine", &resourceType{
		name: propertyStateMachineName,
		properties: []string{
			propertyStateMachineName,
			"StateMachineType",
			memberDefinition,
			"DefinitionString",
			"DefinitionSubstitutions",
			memberRoleArn,
			memberLoggingConfiguration,
			memberTracingConfiguration,
			memberTags,
		},
		attributes: []string{memberArn, memberName},
		replace:    []string{propertyStateMachineName, "StateMachineType"},
		create:     createStateMachine,
		update:     updateStateMachine,
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "sfn", "DeleteStateMachine", map[string]any{memberStateMachineArn: r.Physical})
			if gone(err, "StateMachineDoesNotExist") {
				return nil
			}

			return err
		},
	})
	register("AWS::StepFunctions::Activity", &resourceType{
		properties: []string{memberName, memberTags},
		attributes: []string{memberArn, memberName},
		replace:    []string{memberName},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := map[string]any{memberName: p.str(memberName)}
			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagList(tags, memberKey, memberValue)
			}

			out, err := c.call(ctx, "sfn", "CreateActivity", params)
			if err != nil {
				return "", nil, err
			}

			arn := outText(out, "ActivityArn")
			return arn, map[string]any{memberArn: arn, memberName: p.str(memberName)}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			return r.Attributes, retag(ctx, c, r.Properties, p, sfnTagger(r.Physical))
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "sfn", "DeleteActivity", map[string]any{"ActivityArn": r.Physical})
			if gone(err, "ActivityDoesNotExist") {
				return nil
			}

			return err
		},
	})
	register("AWS::Kinesis::Stream", &resourceType{
		name: memberName,
		properties: []string{
			memberName, "ShardCount", "RetentionPeriodHours", "StreamModeDetails", "StreamEncryption", memberTags,
		},
		attributes: []string{memberArn},
		replace:    []string{memberName},
		create:     createStream,
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if err := configureStream(ctx, c, r, p); err != nil {
				return nil, err
			}

			return r.Attributes, retag(ctx, c, r.Properties, p, &tagger{
				target:  map[string]any{memberStreamName: r.Physical},
				service: "kinesis", tag: "AddTagsToStream", untag: "RemoveTagsFromStream",
			})
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "kinesis", "DeleteStream", map[string]any{
				memberStreamName: r.Physical, "EnforceConsumerDeletion": true,
			})
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
	register("AWS::Events::EventBus", &resourceType{
		properties: []string{memberName, memberDescription, memberTags},
		attributes: []string{memberArn, memberName},
		replace:    []string{memberName},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := pick(map[string]any{memberName: p.str(memberName)}, p, memberDescription)
			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagList(tags, memberKey, memberValue)
			}

			out, err := c.call(ctx, "eventbridge", "CreateEventBus", params)
			if err != nil {
				return "", nil, err
			}

			name := p.str(memberName)
			return name, map[string]any{memberArn: outText(out, "EventBusArn"), memberName: name}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if p.changed(r.Properties, memberDescription) {
				if _, err := c.call(ctx, "eventbridge", "UpdateEventBus", map[string]any{
					memberName: r.Physical, memberDescription: p.str(memberDescription),
				}); err != nil {
					return nil, err
				}
			}

			return r.Attributes, retag(ctx, c, r.Properties, p, &tagger{
				target:  map[string]any{memberResourceARN: outText(r.Attributes, memberArn)},
				service: "eventbridge", tag: opTagResource, untag: opUntagResource, key: memberKey, value: memberValue,
			})
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "eventbridge", "DeleteEventBus", map[string]any{memberName: r.Physical})
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
	register("AWS::CloudWatch::Alarm", &resourceType{
		name:       memberAlarmName,
		properties: append([]string{memberAlarmName, memberTags}, alarmProperties...),
		attributes: []string{memberArn},
		replace:    []string{memberAlarmName},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			name := c.nameOf(p, memberAlarmName, 255)
			params := pick(map[string]any{memberAlarmName: name}, p, alarmProperties...)
			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagList(tags, memberKey, memberValue)
			}

			if _, err := c.call(ctx, "cloudwatch", "PutMetricAlarm", params); err != nil {
				return "", nil, err
			}

			arn := "arn:aws:cloudwatch:" + c.scope.Region + ":" + c.scope.AccountID + ":alarm:" + name
			return name, map[string]any{memberArn: arn}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := pick(map[string]any{memberAlarmName: r.Physical}, p, alarmProperties...)
			if _, err := c.call(ctx, "cloudwatch", "PutMetricAlarm", params); err != nil {
				return nil, err
			}

			return r.Attributes, retag(ctx, c, r.Properties, p, &tagger{
				target:  map[string]any{memberResourceARN: outText(r.Attributes, memberArn)},
				service: "cloudwatch", tag: opTagResource, untag: opUntagResource, key: memberKey, value: memberValue,
			})
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "cloudwatch", "DeleteAlarms", map[string]any{"AlarmNames": []any{r.Physical}})
			return err
		},
	})
}

func sfnTagger(arn string) *tagger {
	return &tagger{
		target:  map[string]any{memberResourceArn: arn},
		service: "sfn", tag: opTagResource, untag: opUntagResource, key: memberKey, value: memberValue,
	}
}

// definition returns a state machine's definition as text, with its
// DefinitionSubstitutions made.
func definition(p props) (string, error) {
	text := p.str("DefinitionString")
	switch {
	case p.has(memberDefinition) && p.has("DefinitionString"):
		return "", validation("You can't specify both Definition and DefinitionString")
	case p.has(memberDefinition):
		text = p.document(memberDefinition)
	case text == "":
		return "", validation("A state machine needs a Definition or a DefinitionString")
	}

	substitutions, _ := p["DefinitionSubstitutions"].(map[string]any)
	for _, k := range slices.Sorted(maps.Keys(substitutions)) {
		text = strings.ReplaceAll(text, "${"+k+"}", scalarText(substitutions[k]))
	}

	return text, nil
}

func createStateMachine(
	ctx context.Context, c *client, p props,
) (physical string, attributes map[string]any, err error) {
	text, err := definition(p)
	if err != nil {
		return "", nil, err
	}

	name := c.nameOf(p, propertyStateMachineName, 80)
	params := pick(map[string]any{memberName: name, memberDefinition: text}, p,
		memberRoleArn, memberLoggingConfiguration, memberTracingConfiguration)
	if p.has("StateMachineType") {
		params[memberType] = p.str("StateMachineType")
	}

	if tags := p.tags(); len(tags) > 0 {
		params[memberTags] = tagList(tags, memberKey, memberValue)
	}

	out, err := c.call(ctx, "sfn", "CreateStateMachine", params)
	if err != nil {
		return "", nil, err
	}

	arn := outText(out, memberStateMachineArn)
	return arn, map[string]any{memberArn: arn, memberName: name}, nil
}

func updateStateMachine(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
	old := props(r.Properties)
	changed := false
	for _, k := range []string{
		memberDefinition, "DefinitionString", "DefinitionSubstitutions", memberRoleArn, memberLoggingConfiguration,
		memberTracingConfiguration,
	} {
		changed = changed || p.changed(old, k)
	}

	if changed {
		text, err := definition(p)
		if err != nil {
			return nil, err
		}

		params := pick(map[string]any{memberStateMachineArn: r.Physical, memberDefinition: text}, p,
			memberRoleArn, memberLoggingConfiguration, memberTracingConfiguration)
		if _, err = c.call(ctx, "sfn", "UpdateStateMachine", params); err != nil {
			return nil, err
		}
	}

	return r.Attributes, retag(ctx, c, old, p, sfnTagger(r.Physical))
}

func createStream(ctx context.Context, c *client, p props) (physical string, attributes map[string]any, err error) {
	name := c.nameOf(p, memberName, 128)
	params := pick(map[string]any{memberStreamName: name}, p, "ShardCount", "StreamModeDetails")
	mode, _ := p["StreamModeDetails"].(map[string]any)
	if !p.has("ShardCount") && scalarText(mode["StreamMode"]) != "ON_DEMAND" {
		params["ShardCount"] = 1.0
	}

	if tags := p.tags(); len(tags) > 0 {
		params[memberTags] = tagMap(tags)
	}

	if _, err = c.call(ctx, "kinesis", "CreateStream", params); err != nil {
		return "", nil, err
	}

	r := &resource{Physical: name, Properties: pick(props{}, p, "ShardCount", "StreamModeDetails")}
	attributes = map[string]any{
		memberArn: "arn:aws:kinesis:" + c.scope.Region + ":" + c.scope.AccountID + ":stream/" + name,
	}
	r.Attributes = attributes
	return name, attributes, configureStream(ctx, c, r, p)
}

// configureStream applies a stream's shard count, mode, retention period,
// and encryption when they change.
func configureStream(ctx context.Context, c *client, r *resource, p props) error {
	old := props(r.Properties)
	if p.changed(old, "StreamModeDetails") {
		mode, _ := p["StreamModeDetails"].(map[string]any)
		if mode == nil {
			mode = map[string]any{"StreamMode": "PROVISIONED"}
		}

		if _, err := c.call(ctx, "kinesis", "UpdateStreamMode", map[string]any{
			"StreamARN": outText(r.Attributes, memberArn), "StreamModeDetails": mode,
		}); err != nil {
			return err
		}
	}

	if p.changed(old, "ShardCount") && p.has("ShardCount") {
		if _, err := c.call(ctx, "kinesis", "UpdateShardCount", map[string]any{
			memberStreamName: r.Physical, "TargetShardCount": p["ShardCount"], "ScalingType": "UNIFORM_SCALING",
		}); err != nil {
			return err
		}
	}

	before, err := retentionHours(old)
	if err != nil {
		return err
	}

	after, err := retentionHours(p)
	if err != nil {
		return err
	}

	if before != after {
		operation := "IncreaseStreamRetentionPeriod"
		if after < before {
			operation = "DecreaseStreamRetentionPeriod"
		}

		_, err = c.call(ctx, "kinesis", operation, map[string]any{
			memberStreamName: r.Physical, "RetentionPeriodHours": float64(after),
		})
		if err != nil {
			return err
		}
	}

	if !p.changed(old, "StreamEncryption") {
		return nil
	}

	operation, encryption := "StartStreamEncryption", p["StreamEncryption"]
	if encryption == nil {
		operation, encryption = "StopStreamEncryption", old["StreamEncryption"]
	}

	settings, _ := encryption.(map[string]any)
	params := pick(map[string]any{memberStreamName: r.Physical}, settings, "EncryptionType", "KeyId")
	_, err = c.call(ctx, "kinesis", operation, params)
	return err
}

// retentionHours returns a stream's RetentionPeriodHours, or the default.
func retentionHours(p props) (int, error) {
	if !p.has("RetentionPeriodHours") {
		return defaultRetention, nil
	}

	n, err := strconv.Atoi(p.str("RetentionPeriodHours"))
	if err != nil {
		return 0, validation("RetentionPeriodHours must be a whole number of hours")
	}

	return n, nil
}

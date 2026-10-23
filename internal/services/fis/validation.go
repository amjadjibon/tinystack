package fis

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/fis"
	"github.com/amjadjibon/tinystack/internal/common"
)

var (
	durationPattern  = regexp.MustCompile(`^PT(?:(\d+(?:\.\d+)?)H)?(?:(\d+(?:\.\d+)?)M)?(?:(\d+(?:\.\d+)?)S)?$`)
	selectionPattern = regexp.MustCompile(`^(COUNT|PERCENT)\((\d+)\)$`)
)

func waitDuration(value string) (time.Duration, error) {
	parts := durationPattern.FindStringSubmatch(value)
	if parts == nil || value == "PT" {
		return 0, awserror.Validation("duration must be an ISO 8601 time duration")
	}

	seconds := float64(0)
	for i, unit := range []float64{3600, 60, 1} {
		if parts[i+1] == "" {
			continue
		}

		n, err := strconv.ParseFloat(parts[i+1], 64)
		if err != nil {
			return 0, awserror.Validation("Invalid duration")
		}

		seconds += n * unit
	}

	if math.IsInf(seconds, 0) || seconds < 60 || seconds > 12*3600 {
		return 0, awserror.Validation("aws:fis:wait duration must be between one minute and 12 hours")
	}

	return time.Duration(seconds * float64(time.Second)), nil
}

func validateTemplate(t *api.ExperimentTemplate) error {
	visited := map[string]int{}
	var visit func(string) error
	visit = func(name string) error {
		a, ok := t.Actions[name]
		if !ok {
			return awserror.Validation("startAfter references unknown action %q", name)
		}

		if visited[name] == 1 {
			return awserror.Validation("startAfter contains a cycle at action %q", name)
		}

		if visited[name] == 2 {
			return nil
		}

		visited[name] = 1
		for _, dep := range a.StartAfter {
			if err := visit(dep); err != nil {
				return err
			}
		}

		visited[name] = 2
		return nil
	}

	for name, a := range t.Actions {
		if common.Value(a.ActionId) != waitAction {
			return awserror.Validation("Action %q uses unsupported actionId %q", name, common.Value(a.ActionId))
		}

		if _, err := waitDuration(a.Parameters["duration"]); err != nil {
			return err
		}

		for key := range a.Parameters {
			if key != "duration" {
				return awserror.Validation("Unknown parameter %q for action %q", key, name)
			}
		}

		for _, target := range a.Targets {
			if _, ok := t.Targets[target]; !ok {
				return awserror.Validation("Action %q references unknown target %q", name, target)
			}
		}

		if len(a.Targets) != 0 {
			return awserror.Validation("Action %q (%s) does not support targets", name, waitAction)
		}

		if err := visit(name); err != nil {
			return err
		}
	}

	for name, target := range t.Targets {
		mode := common.Value(target.SelectionMode)
		if mode != "ALL" {
			parts := selectionPattern.FindStringSubmatch(mode)
			if parts == nil {
				return awserror.Validation("Invalid selectionMode for target %q", name)
			}

			n, err := strconv.ParseUint(parts[2], 10, 64)
			if err != nil || n == 0 || parts[1] == "PERCENT" && n > 100 {
				return awserror.Validation("Invalid selectionMode for target %q", name)
			}
		}

		if len(target.ResourceArns) > 0 && (len(target.ResourceTags) > 0 || len(target.Filters) > 0) {
			return awserror.Validation("Target %q cannot combine resourceArns with resourceTags or filters", name)
		}
	}

	for _, stop := range t.StopConditions {
		switch common.Value(stop.Source) {
		case "none":
			if stop.Value != nil || len(t.StopConditions) != 1 {
				return awserror.Validation("none must be the sole stop condition without a value")
			}
		case alarmSource:
			if stop.Value == nil {
				return awserror.Validation("A CloudWatch stop condition requires an alarm ARN")
			}
		default:
			return awserror.Validation("Unsupported stop condition source %q", common.Value(stop.Source))
		}
	}

	return nil
}

func (s *Service) validateReferences(ctx context.Context, t *api.ExperimentTemplate) error {
	arn := common.Value(t.RoleArn)
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) != 6 || parts[2] != "iam" || !strings.HasPrefix(parts[5], "role/") {
		return awserror.Validation("roleArn must reference an existing IAM role")
	}

	out, err := s.caller.Call(ctx, "iam", "GetRole", map[string]any{"RoleName": arn[strings.LastIndex(arn, "/")+1:]})
	if err != nil {
		return awserror.Validation("roleArn does not reference an existing IAM role: %s", arn)
	}

	role, _ := out["Role"].(map[string]any)
	if role["Arn"] != arn {
		return awserror.Validation("roleArn does not reference an existing IAM role: %s", arn)
	}

	for _, stop := range t.StopConditions {
		if common.Value(stop.Source) != alarmSource {
			continue
		}

		if _, err := s.alarmState(ctx, common.Value(stop.Value)); err != nil {
			return err
		}
	}

	log := t.LogConfiguration
	if log == nil {
		return nil
	}

	if common.Value(log.LogSchemaVersion) != 1 && common.Value(log.LogSchemaVersion) != 2 {
		return awserror.Validation("logSchemaVersion must be 1 or 2")
	}

	if log.CloudWatchLogsConfiguration != nil {
		arn := common.Value(log.CloudWatchLogsConfiguration.LogGroupArn)
		parts := strings.SplitN(arn, ":log-group:", 2)
		if len(parts) != 2 {
			return awserror.Validation("Invalid CloudWatch Logs group ARN")
		}

		name := strings.TrimSuffix(parts[1], ":*")
		out, err := s.caller.Call(ctx, "logs", "DescribeLogGroups", map[string]any{"LogGroupNamePrefix": name})
		if err != nil {
			return awserror.Validation("Log group does not exist: %s", arn)
		}

		found := false
		groups, _ := out["LogGroups"].([]any)
		for _, item := range groups {
			group := item.(map[string]any)
			if group["LogGroupName"] == name && (group["Arn"] == arn || group["Arn"] == arn+":*") {
				found = true
			}
		}

		if !found {
			return awserror.Validation("Log group does not exist: %s", arn)
		}
	}

	if log.S3Configuration != nil {
		_, err := s.caller.Call(
			ctx,
			"s3",
			"HeadBucket",
			map[string]any{"Bucket": common.Value(log.S3Configuration.BucketName)},
		)
		if err != nil {
			return awserror.Validation("Log destination bucket does not exist")
		}
	}

	return nil
}

func (s *Service) alarmState(ctx context.Context, arn string) (string, error) {
	parts := strings.SplitN(arn, ":alarm:", 2)
	if len(parts) != 2 {
		return "", awserror.Validation("Invalid CloudWatch alarm ARN: %s", arn)
	}

	out, err := s.caller.Call(ctx, "cloudwatch", "DescribeAlarms", map[string]any{
		"AlarmNames": []any{parts[1]}, "AlarmTypes": []any{"MetricAlarm", "CompositeAlarm"},
	})
	if err != nil {
		return "", fmt.Errorf("describe stop-condition alarm: %w", err)
	}

	for _, field := range []string{"MetricAlarms", "CompositeAlarms"} {
		items, _ := out[field].([]any)
		for _, item := range items {
			alarm := item.(map[string]any)
			if alarm["AlarmArn"] == arn {
				state, _ := alarm["StateValue"].(string)
				return state, nil
			}
		}
	}

	return "", awserror.Validation("Stop-condition CloudWatch alarm does not exist: %s", arn)
}

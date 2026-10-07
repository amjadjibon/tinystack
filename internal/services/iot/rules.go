package iot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/iot"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindRule = "rule"
	// fieldRole is the action member that names its IAM role.
	fieldRole = "roleArn"
	// keyTopic names a message's topic in payloads and logs.
	keyTopic = "topic"
	// maxActions is the most actions a rule may have.
	maxActions = 10
	// defaultRulePage and maxRulePage bound ListTopicRules' pages.
	defaultRulePage = 1000
	maxRulePage     = 10000
)

var (
	ruleNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_]{1,128}$`)
	rolePattern     = regexp.MustCompile(`^arn:aws:iam::\d{12}:role/[\w+=,.@/-]+$`)
)

// requiredFields are the members each action must set, by the action's JSON
// name.
var requiredFields = map[string][]string{
	"dynamoDB":         {"tableName", fieldRole, "hashKeyField", "hashKeyValue"},
	"dynamoDBv2":       {fieldRole},
	"lambda":           {"functionArn"},
	"sns":              {"targetArn", fieldRole},
	"sqs":              {fieldRole, "queueUrl"},
	"kinesis":          {fieldRole, "streamName"},
	"republish":        {fieldRole, keyTopic},
	"s3":               {fieldRole, "bucketName", "key"},
	"firehose":         {fieldRole, "deliveryStreamName"},
	"cloudwatchMetric": {fieldRole, "metricNamespace", "metricName", "metricValue", "metricUnit"},
	"cloudwatchAlarm":  {fieldRole, "alarmName", "stateReason", "stateValue"},
	"cloudwatchLogs":   {fieldRole, "logGroupName"},
	"stepFunctions":    {"stateMachineName", fieldRole},
}

// topicRule is a topic rule's record.
type topicRule struct {
	Tags    map[string]string
	Payload api.TopicRulePayload
	Name    string
	Created float64
}

func ruleKey(scope request.Scope, name string) string { return scope.Key(service, kindRule, name) }

// ruleNotFound is how IoT reports a rule that does not exist.
func ruleNotFound(name string) error {
	return awserror.New("UnauthorizedException", fmt.Sprintf("The topic rule %s does not exist.", name))
}

func loadRule(tx storage.ReadTx, scope request.Scope, name string) (*topicRule, error) {
	r, found, err := storage.Read[topicRule](tx, ruleKey(scope, name))
	if err == nil && !found {
		err = ruleNotFound(name)
	}

	return r, err
}

// checkRule validates a rule's payload: its SQL, version, and actions.
func checkRule(p *api.TopicRulePayload) error {
	if p == nil {
		return invalid("The topic rule payload is required.")
	}

	if _, err := parseQuery(common.Value(p.Sql)); err != nil {
		return err
	}

	if p.AwsIotSqlVersion != nil && !slices.Contains(sqlVersions, *p.AwsIotSqlVersion) {
		return sqlError("The SQL version %s is not supported; use one of %s.", *p.AwsIotSqlVersion,
			strings.Join(sqlVersions, ", "))
	}

	if len(p.Actions) > maxActions {
		return invalid("A topic rule can have at most %d actions.", maxActions)
	}

	for i := range p.Actions {
		if _, _, err := actionFields(&p.Actions[i]); err != nil {
			return err
		}
	}

	if p.ErrorAction != nil {
		if _, _, err := actionFields(p.ErrorAction); err != nil {
			return err
		}
	}

	return nil
}

// actionFields returns the kind of action an Action sets, which must be
// exactly one, and its members by JSON name, checking the required ones,
// the role, and the substitution templates.
func actionFields(a *api.Action) (kind string, fields map[string]any, err error) {
	data, err := json.Marshal(a)
	if err != nil {
		return "", nil, err
	}

	var set map[string]map[string]any
	if unmarshalErr := json.Unmarshal(data, &set); unmarshalErr != nil {
		return "", nil, unmarshalErr
	}

	if len(set) != 1 {
		return "", nil, invalid("Each action must set exactly one kind of action.")
	}

	for k, f := range set {
		kind, fields = k, f
	}

	for _, name := range requiredFields[kind] {
		if s, ok := fields[name].(string); !ok || s == "" {
			return "", nil, invalid("The %s action needs %s.", kind, name)
		}
	}

	if kind == "dynamoDBv2" {
		item, _ := fields["putItem"].(map[string]any)
		if s, _ := item["tableName"].(string); s == "" {
			return "", nil, invalid("The dynamoDBv2 action needs putItem.tableName.")
		}
	}

	if role, ok := fields[fieldRole].(string); ok && !rolePattern.MatchString(role) {
		return "", nil, invalid("The role %s is not an IAM role ARN.", role)
	}

	err = walkStrings(fields, func(name, s string) (string, error) {
		if name == fieldRole {
			return s, nil
		}

		_, parseErr := parseTemplate(s)
		return s, parseErr
	})
	if err != nil {
		return "", nil, invalid("%v", err)
	}

	return kind, fields, nil
}

// walkStrings replaces each string in a decoded document with fn's result,
// passing the name of the member that holds it.
func walkStrings(v map[string]any, fn func(name, s string) (string, error)) error {
	for k, e := range v {
		switch x := e.(type) {
		case string:
			s, err := fn(k, x)
			if err != nil {
				return err
			}

			v[k] = s
		case map[string]any:
			if err := walkStrings(x, fn); err != nil {
				return err
			}
		case []any:
			for _, item := range x {
				if m, ok := item.(map[string]any); ok {
					if err := walkStrings(m, fn); err != nil {
						return err
					}
				}
			}
		}
	}

	return nil
}

// ruleTags parses CreateTopicRule's x-amz-tagging header, k1=v1&k2=v2.
func ruleTags(header *string) (map[string]string, error) {
	tags := map[string]string{}
	if header == nil {
		return tags, nil
	}

	values, err := url.ParseQuery(*header)
	if err != nil {
		return nil, invalid("The tags are not URL-encoded key=value pairs.")
	}

	for k, v := range values {
		tags[k] = v[len(v)-1]
	}

	if err := tagging.CheckSet(tags); err != nil {
		return nil, invalid("%s", err.Error())
	}

	return tags, nil
}

func (s *Service) CreateTopicRule(
	ctx context.Context,
	in *api.CreateTopicRuleRequest,
) (*api.CreateTopicRuleOutput, error) {
	name := common.Value(in.RuleName)
	if !ruleNamePattern.MatchString(name) {
		return nil, invalid("The rule name must be 1 to 128 letters, digits, or underscores.")
	}

	if err := checkRule(in.TopicRulePayload); err != nil {
		return nil, err
	}

	tags, err := ruleTags(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if exists(tx, ruleKey(scope, name)) {
			return alreadyExists("The topic rule %s already exists.", name)
		}

		return storage.Write(tx, ruleKey(scope, name), &topicRule{
			Name: name, Payload: *in.TopicRulePayload, Tags: tags, Created: common.Unix(s.now()),
		})
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateTopicRuleOutput{}, nil
}

func (s *Service) GetTopicRule(ctx context.Context, in *api.GetTopicRuleRequest) (*api.GetTopicRuleResponse, error) {
	var out *api.GetTopicRuleResponse
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		r, err := loadRule(tx, scope, common.Value(in.RuleName))
		if err != nil {
			return err
		}

		p := r.Payload
		out = &api.GetTopicRuleResponse{
			RuleArn: new(arn(scope, kindRule, r.Name)),
			Rule: &api.TopicRule{
				RuleName: new(r.Name), Sql: p.Sql, Description: p.Description, CreatedAt: new(r.Created),
				Actions: p.Actions, RuleDisabled: new(common.Value(p.RuleDisabled)),
				AwsIotSqlVersion: new(common.Value(common.OrElse(p.AwsIotSqlVersion, new(sqlVersions[0])))),
				ErrorAction:      p.ErrorAction,
			},
		}
		if out.Rule.Actions == nil {
			out.Rule.Actions = []api.Action{}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// changeRule applies fn to an existing rule's record.
func (s *Service) changeRule(ctx context.Context, name *string, fn func(*topicRule)) error {
	return s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := loadRule(tx, scope, common.Value(name))
		if err != nil {
			return err
		}

		fn(r)
		return storage.Write(tx, ruleKey(scope, r.Name), r)
	})
}

func (s *Service) ReplaceTopicRule(
	ctx context.Context,
	in *api.ReplaceTopicRuleRequest,
) (*api.ReplaceTopicRuleOutput, error) {
	if err := checkRule(in.TopicRulePayload); err != nil {
		return nil, err
	}

	if err := s.changeRule(ctx, in.RuleName, func(r *topicRule) { r.Payload = *in.TopicRulePayload }); err != nil {
		return nil, err
	}

	return &api.ReplaceTopicRuleOutput{}, nil
}

func (s *Service) EnableTopicRule(
	ctx context.Context,
	in *api.EnableTopicRuleRequest,
) (*api.EnableTopicRuleOutput, error) {
	if err := s.changeRule(ctx, in.RuleName, func(r *topicRule) { r.Payload.RuleDisabled = new(false) }); err != nil {
		return nil, err
	}

	return &api.EnableTopicRuleOutput{}, nil
}

func (s *Service) DisableTopicRule(
	ctx context.Context,
	in *api.DisableTopicRuleRequest,
) (*api.DisableTopicRuleOutput, error) {
	if err := s.changeRule(ctx, in.RuleName, func(r *topicRule) { r.Payload.RuleDisabled = new(true) }); err != nil {
		return nil, err
	}

	return &api.DisableTopicRuleOutput{}, nil
}

func (s *Service) DeleteTopicRule(
	ctx context.Context,
	in *api.DeleteTopicRuleRequest,
) (*api.DeleteTopicRuleOutput, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := loadRule(tx, scope, common.Value(in.RuleName)); err != nil {
			return err
		}

		tx.Delete(ruleKey(scope, common.Value(in.RuleName)))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteTopicRuleOutput{}, nil
}

func (s *Service) ListTopicRules(
	ctx context.Context,
	in *api.ListTopicRulesRequest,
) (*api.ListTopicRulesResponse, error) {
	n := int(common.Value(in.MaxResults))
	switch {
	case in.MaxResults == nil:
		n = defaultRulePage
	case n < 1 || n > maxRulePage:
		return nil, invalid("maxResults must be between 1 and %d.", maxRulePage)
	}

	var items []api.TopicRuleListItem
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		rules, err := storage.Records[topicRule](tx, scope.Prefix(service, kindRule))
		for _, r := range rules {
			q, parseErr := parseQuery(common.Value(r.Payload.Sql))
			if parseErr != nil {
				return parseErr
			}

			disabled := common.Value(r.Payload.RuleDisabled)
			if in.Topic != nil && q.topic != *in.Topic && !topicMatches(q.topic, *in.Topic) ||
				in.RuleDisabled != nil && *in.RuleDisabled != disabled {
				continue
			}

			items = append(items, api.TopicRuleListItem{
				RuleArn: new(arn(scope, kindRule, r.Name)), RuleName: new(r.Name), TopicPattern: new(q.topic),
				CreatedAt: new(r.Created), RuleDisabled: new(disabled),
			})
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := common.Page(items, func(r api.TopicRuleListItem) string { return *r.RuleName },
		in.NextToken, n, invalid("The pagination token is invalid."))
	if err != nil {
		return nil, err
	}

	if selected == nil {
		selected = []api.TopicRuleListItem{}
	}

	return &api.ListTopicRulesResponse{Rules: selected, NextToken: next}, nil
}

// topicMatches reports whether an MQTT topic filter matches a topic. As in
// MQTT, wildcards at the first level do not match topics that start with $.
func topicMatches(filter, topic string) bool {
	if strings.HasPrefix(topic, "$") && (strings.HasPrefix(filter, "+") || strings.HasPrefix(filter, "#")) {
		return false
	}

	f, t := strings.Split(filter, "/"), strings.Split(topic, "/")
	for i, level := range f {
		switch {
		case level == "#":
			return true
		case i >= len(t):
			return false
		case level != "+" && level != t[i]:
			return false
		}
	}

	return len(f) == len(t)
}

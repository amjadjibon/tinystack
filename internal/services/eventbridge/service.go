// Package eventbridge implements rules and targets on each account's default
// event bus, without HTTP dependencies. PutEvents matches events against rule
// patterns and delivers them to SQS, SNS, and Lambda targets in-process.
//
// Each rule, with its targets, is one record under
// "<account>/<region>/eventbridge/rule/<name>".
package eventbridge

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/eventbridge"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/lambda"
	"github.com/amjadjibon/tinystack/internal/services/sns"
	"github.com/amjadjibon/tinystack/internal/services/sqs"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	defaultBus    = "default"
	stateEnabled  = "ENABLED"
	stateDisabled = "DISABLED"
	maxRules      = 300
	maxTargets    = 5
	pageSize      = 100
	serviceSQS    = "sqs"
	serviceSNS    = "sns"
	serviceLambda = "lambda"
)

var _ api.API = (*Service)(nil)

type Service struct {
	store     storage.Store
	queues    *sqs.Service
	topics    *sns.Service
	functions *lambda.Service
	now       func() time.Time
}

func New(store storage.Store, queues *sqs.Service, topics *sns.Service, functions *lambda.Service) *Service {
	return &Service{store: store, queues: queues, topics: topics, functions: functions, now: time.Now}
}

type target struct {
	ID             string
	ARN            string
	RoleARN        string `json:",omitempty"`
	Input          string `json:",omitempty"`
	InputPath      string `json:",omitempty"`
	MessageGroupID string `json:",omitempty"`
}

type rule struct {
	Name        string
	Pattern     string
	State       string
	Description string `json:",omitempty"`
	RoleARN     string `json:",omitempty"`
	Targets     []target
}

var (
	ruleName  = regexp.MustCompile(`^[.\-_A-Za-z0-9]{1,64}$`)
	targetID  = ruleName
	accountID = regexp.MustCompile(`^\d{12}$`)
	// inputPath is the JSONPath subset TinyStack supports: "$" or dotted fields.
	inputPath = regexp.MustCompile(`^\$(\.[A-Za-z0-9_-]+)*$`)
)

func ptr[T any](v T) *T { return &v }

func value[T any](p *T) (v T) {
	if p != nil {
		return *p
	}

	return v
}

func invalid(format string, args ...any) error {
	return awserror.Validation(format, args...)
}

func ruleNotFound(name string) error {
	return awserror.New("ResourceNotFoundException", "Rule "+name+" does not exist on EventBus default.")
}

func identity(ctx context.Context) (request.Metadata, error) {
	m, ok := request.FromContext(ctx)
	if !ok || m.AccountID == "" || m.Region == "" {
		return m, fmt.Errorf("missing request identity")
	}

	return m, nil
}

func rulePrefix(scope request.Scope) string {
	return scope.AccountID + "/" + scope.Region + "/eventbridge/rule/"
}

func ruleARN(scope request.Scope, name string) string {
	return "arn:aws:events:" + scope.Region + ":" + scope.AccountID + ":rule/" + name
}

// checkBus accepts the default bus by name or ARN. TinyStack has no custom
// event buses, so any other bus does not exist.
func checkBus(scope request.Scope, name *string) error {
	bus := value(name)
	if bus == "" || bus == defaultBus ||
		bus == "arn:aws:events:"+scope.Region+":"+scope.AccountID+":event-bus/"+defaultBus {
		return nil
	}

	return awserror.New("ResourceNotFoundException", "Event bus "+bus+" does not exist.")
}

// resolveRule validates a rule reference and returns its storage key.
func resolveRule(ctx context.Context, name, bus *string) (request.Metadata, string, error) {
	m, err := identity(ctx)
	if err != nil {
		return m, "", err
	}

	if !ruleName.MatchString(value(name)) {
		return m, "", invalid("Rule name must be 1 to 64 characters from [.-_A-Za-z0-9].")
	}

	if busErr := checkBus(m.Scope, bus); busErr != nil {
		return m, "", busErr
	}

	return m, rulePrefix(m.Scope) + *name, nil
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	s := hex.EncodeToString(b[:])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}

func load(tx storage.ReadTx, key string) (*rule, bool, error) {
	data, ok := tx.Get(key)
	if !ok {
		return nil, false, nil
	}

	var r rule
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, false, fmt.Errorf("decode rule: %w", err)
	}

	return &r, true, nil
}

func save(tx storage.WriteTx, key string, r *rule) error {
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encode rule: %w", err)
	}

	tx.Put(key, data)
	return nil
}

// update loads an existing rule, applies fn, and saves it.
func (s *Service) update(ctx context.Context, name, key string, fn func(r *rule) error) error {
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, ok, err := load(tx, key)
		if err != nil {
			return err
		}

		if !ok {
			return ruleNotFound(name)
		}

		if fnErr := fn(r); fnErr != nil {
			return fnErr
		}

		return save(tx, key, r)
	})
}

// page decodes a NextToken, the last name of the previous page, and checks a limit.
func page(token *string, limit *int64) (after string, size int, err error) {
	size = pageSize
	if limit != nil {
		if *limit < 1 || *limit > pageSize {
			return "", 0, invalid("Limit must be between 1 and %d.", pageSize)
		}

		size = int(*limit)
	}

	if token == nil {
		return "", size, nil
	}

	b, err := base64.RawURLEncoding.DecodeString(*token)
	if err != nil || len(b) == 0 {
		return "", 0, invalid("The NextToken is not valid.")
	}

	return string(b), size, nil
}

func nextToken(last string) *string {
	return ptr(base64.RawURLEncoding.EncodeToString([]byte(last)))
}

func (s *Service) PutRule(ctx context.Context, in *api.PutRuleRequest) (*api.PutRuleResponse, error) {
	m, key, err := resolveRule(ctx, in.Name, in.EventBusName)
	if err != nil {
		return nil, err
	}

	text := value(in.EventPattern)
	if text == "" {
		return nil, invalid("Parameter(s) EventPattern or ScheduleExpression must be specified.")
	}

	if len(text) > 4096 {
		return nil, invalid("EventPattern must be at most 4096 characters.")
	}

	if _, err = compilePattern(text); err != nil {
		return nil, err
	}

	state := value(in.State)
	switch state {
	case "":
		state = stateEnabled
	case stateEnabled, stateDisabled:
	default:
		return nil, invalid("State must be ENABLED or DISABLED.")
	}

	if len(value(in.Description)) > 512 {
		return nil, invalid("Description must be at most 512 characters.")
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, exists, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		if !exists {
			count := 0
			for _, k := range tx.Keys() {
				if strings.HasPrefix(k, rulePrefix(m.Scope)) {
					count++
				}
			}

			if count >= maxRules {
				return awserror.New(
					"LimitExceededException",
					"The requested resource exceeds the maximum number allowed.",
				)
			}

			r = &rule{Name: *in.Name}
		}

		r.Pattern, r.State = text, state
		r.Description, r.RoleARN = value(in.Description), value(in.RoleArn)
		return save(tx, key, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.PutRuleResponse{RuleArn: ptr(ruleARN(m.Scope, *in.Name))}, nil
}

func (s *Service) DescribeRule(ctx context.Context, in *api.DescribeRuleRequest) (*api.DescribeRuleResponse, error) {
	m, key, err := resolveRule(ctx, in.Name, in.EventBusName)
	if err != nil {
		return nil, err
	}

	var r *rule
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var ok bool
		r, ok, err = load(tx, key)
		if err == nil && !ok {
			return ruleNotFound(*in.Name)
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	out := r.describe(m.Scope)
	return &api.DescribeRuleResponse{
		Name:         out.Name,
		Arn:          out.Arn,
		EventPattern: out.EventPattern,
		State:        out.State,
		Description:  out.Description,
		RoleArn:      out.RoleArn,
		EventBusName: out.EventBusName,
		CreatedBy:    ptr(m.AccountID),
	}, nil
}

func (r *rule) describe(scope request.Scope) api.Rule {
	out := api.Rule{
		Name:         ptr(r.Name),
		Arn:          ptr(ruleARN(scope, r.Name)),
		EventPattern: ptr(r.Pattern),
		State:        ptr(r.State),
		EventBusName: ptr(defaultBus),
	}
	if r.Description != "" {
		out.Description = ptr(r.Description)
	}

	if r.RoleARN != "" {
		out.RoleArn = ptr(r.RoleARN)
	}

	return out
}

// DeleteRule succeeds for a rule that does not exist, as EventBridge does.
func (s *Service) DeleteRule(ctx context.Context, in *api.DeleteRuleRequest) (*api.DeleteRuleOutput, error) {
	_, key, err := resolveRule(ctx, in.Name, in.EventBusName)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, ok, loadErr := load(tx, key)
		if loadErr != nil || !ok {
			return loadErr
		}

		if len(r.Targets) > 0 {
			return invalid("Rule can't be deleted since it has targets.")
		}

		tx.Delete(key)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteRuleOutput{}, nil
}

func (s *Service) ListRules(ctx context.Context, in *api.ListRulesRequest) (*api.ListRulesResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if busErr := checkBus(m.Scope, in.EventBusName); busErr != nil {
		return nil, busErr
	}

	after, size, err := page(in.NextToken, in.Limit)
	if err != nil {
		return nil, err
	}

	var rules []*rule
	prefix := rulePrefix(m.Scope) + value(in.NamePrefix)
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys() {
			if !strings.HasPrefix(k, prefix) || strings.TrimPrefix(k, rulePrefix(m.Scope)) <= after {
				continue
			}

			r, _, loadErr := load(tx, k)
			if loadErr != nil {
				return loadErr
			}

			rules = append(rules, r)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(rules, func(i, j int) bool { return rules[i].Name < rules[j].Name })
	out := &api.ListRulesResponse{}
	if len(rules) > size {
		rules = rules[:size]
		out.NextToken = nextToken(rules[size-1].Name)
	}

	for _, r := range rules {
		out.Rules = append(out.Rules, r.describe(m.Scope))
	}

	return out, nil
}

func (s *Service) setState(ctx context.Context, name, bus *string, state string) error {
	_, key, err := resolveRule(ctx, name, bus)
	if err != nil {
		return err
	}

	return s.update(ctx, *name, key, func(r *rule) error {
		r.State = state
		return nil
	})
}

func (s *Service) EnableRule(ctx context.Context, in *api.EnableRuleRequest) (*api.EnableRuleOutput, error) {
	if err := s.setState(ctx, in.Name, in.EventBusName, stateEnabled); err != nil {
		return nil, err
	}

	return &api.EnableRuleOutput{}, nil
}

func (s *Service) DisableRule(ctx context.Context, in *api.DisableRuleRequest) (*api.DisableRuleOutput, error) {
	if err := s.setState(ctx, in.Name, in.EventBusName, stateDisabled); err != nil {
		return nil, err
	}

	return &api.DisableRuleOutput{}, nil
}

func (s *Service) TestEventPattern(
	_ context.Context,
	in *api.TestEventPatternRequest,
) (*api.TestEventPatternResponse, error) {
	p, err := compilePattern(value(in.EventPattern))
	if err != nil {
		return nil, err
	}

	event, ok := flatten([]byte(value(in.Event)))
	if !ok {
		return nil, invalid("Parameter Event is not valid.")
	}

	return &api.TestEventPatternResponse{Result: ptr(p.matches(event))}, nil
}

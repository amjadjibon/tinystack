// Package eventbridge implements event buses with their rules and targets,
// without HTTP dependencies. PutEvents matches events against the rule
// patterns of their bus and delivers them to SQS, SNS, and Lambda targets
// in-process; the scheduler fires scheduled rules (schedule.go).
//
// Each rule, with its targets, is one record under
// "<account>/<region>/eventbridge/rule/<name>" on the default bus, which every
// account has, or "<account>/<region>/eventbridge/bus-rule/<bus>/<name>" on a
// custom bus, whose record is "<account>/<region>/eventbridge/bus/<bus>".
package eventbridge

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/eventbridge"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/services/lambda"
	"github.com/amjadjibon/tinystack/internal/services/pattern"
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
	store      storage.Store
	queues     *sqs.Service
	topics     *sns.Service
	functions  *lambda.Service
	deliveries *delivery.Log
	logger     *slog.Logger
	now        func() time.Time
	// ctx ends the scheduler on Stop.
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	stop   sync.Once
}

// New returns an EventBridge service and starts its scheduler, which runs
// until Stop.
func New(
	store storage.Store,
	queues *sqs.Service,
	topics *sns.Service,
	functions *lambda.Service,
	deliveries *delivery.Log,
) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		store:      store,
		queues:     queues,
		topics:     topics,
		functions:  functions,
		deliveries: deliveries,
		logger:     slog.Default(),
		now:        time.Now,
		ctx:        ctx,
		cancel:     cancel,
		done:       make(chan struct{}),
	}
	go s.scheduler()
	return s
}

// Stop ends the scheduler and waits for it. Later calls return at once.
func (s *Service) Stop() {
	s.stop.Do(func() {
		s.cancel()
		<-s.done
	})
}

type target struct {
	Transformer *transformer `json:",omitempty"`
	// MaxRetries and MaxAge are the RetryPolicy members that were set.
	MaxRetries     *int64 `json:",omitempty"`
	MaxAge         *int64 `json:",omitempty"`
	ID             string
	ARN            string
	RoleARN        string `json:",omitempty"`
	Input          string `json:",omitempty"`
	InputPath      string `json:",omitempty"`
	MessageGroupID string `json:",omitempty"`
	DeadLetterARN  string `json:",omitempty"`
}

type rule struct {
	// Bus is the custom bus of the rule; it is empty on the default bus.
	Bus     string `json:",omitempty"`
	Name    string
	Pattern string `json:",omitempty"`
	// Schedule is the rule's ScheduleExpression; only default bus rules have one.
	Schedule    string `json:",omitempty"`
	State       string
	Description string            `json:",omitempty"`
	RoleARN     string            `json:",omitempty"`
	Tags        map[string]string `json:",omitempty"`
	Targets     []target
}

var (
	ruleName  = regexp.MustCompile(`^[.\-_A-Za-z0-9]{1,64}$`)
	targetID  = ruleName
	accountID = regexp.MustCompile(`^\d{12}$`)
	// inputPath is the JSONPath subset TinyStack supports: "$" followed by
	// dotted fields and array indexes.
	inputPath = regexp.MustCompile(`^\$(\.[A-Za-z0-9_-]+|\[\d+\])*$`)
)

func invalid(format string, args ...any) error {
	return awserror.Validation(format, args...)
}

func ruleNotFound(name, bus string) error {
	return awserror.New("ResourceNotFoundException", "Rule "+name+" does not exist on EventBus "+bus+".")
}

// rulePrefix is the storage prefix of the rules of a bus.
func rulePrefix(scope request.Scope, bus string) string {
	if bus == defaultBus {
		return scope.AccountID + "/" + scope.Region + "/eventbridge/rule/"
	}

	return scope.AccountID + "/" + scope.Region + "/eventbridge/bus-rule/" + bus + "/"
}

// ruleARN is the ARN of a rule; the bus is part of it except for the default
// bus.
func ruleARN(scope request.Scope, bus, name string) string {
	if bus != defaultBus {
		name = bus + "/" + name
	}

	return "arn:aws:events:" + scope.Region + ":" + scope.AccountID + ":rule/" + name
}

// ruleRef is a rule a request names, on its bus.
type ruleRef struct {
	scope          request.Scope
	bus, name, key string
}

func (r *ruleRef) arn() string { return ruleARN(r.scope, r.bus, r.name) }

func (r *ruleRef) notFound() error { return ruleNotFound(r.name, r.bus) }

// resolveRule validates a rule reference on a bus given by name or ARN, the
// default bus when it is absent.
func resolveRule(ctx context.Context, name, bus *string) (ruleRef, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return ruleRef{}, err
	}

	if !ruleName.MatchString(common.Value(name)) {
		return ruleRef{}, invalid("Rule name must be 1 to 64 characters from [.-_A-Za-z0-9].")
	}

	b, err := busName(m.Scope, bus)
	if err != nil {
		return ruleRef{}, err
	}

	return ruleRef{scope: m.Scope, bus: b, name: *name, key: rulePrefix(m.Scope, b) + *name}, nil
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	s := hex.EncodeToString(b[:])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}

// update loads an existing rule, applies fn, and saves it.
func (s *Service) update(ctx context.Context, ref *ruleRef, fn func(r *rule) error) error {
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := loadRule(tx, ref)
		if err != nil {
			return err
		}

		if fnErr := fn(r); fnErr != nil {
			return fnErr
		}

		return storage.Write(tx, ref.key, r)
	})
}

// loadRule reads an existing rule on an existing bus.
func loadRule(tx storage.ReadTx, ref *ruleRef) (*rule, error) {
	if err := checkBus(tx, ref.scope, ref.bus); err != nil {
		return nil, err
	}

	r, ok, err := storage.Read[rule](tx, ref.key)
	if err == nil && !ok {
		return nil, ref.notFound()
	}

	return r, err
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
	return new(base64.RawURLEncoding.EncodeToString([]byte(last)))
}

func (s *Service) PutRule(ctx context.Context, in *api.PutRuleRequest) (*api.PutRuleResponse, error) {
	ref, err := resolveRule(ctx, in.Name, in.EventBusName)
	if err != nil {
		return nil, err
	}

	text, expression := common.Value(in.EventPattern), common.Value(in.ScheduleExpression)
	if text == "" && expression == "" {
		return nil, invalid("Parameter(s) EventPattern or ScheduleExpression must be specified.")
	}

	if len(text) > 4096 {
		return nil, invalid("EventPattern must be at most 4096 characters.")
	}

	if text != "" {
		if _, err = compilePattern(text); err != nil {
			return nil, err
		}
	}

	if expression != "" {
		if ref.bus != defaultBus {
			return nil, invalid("ScheduleExpression is supported only on the default event bus.")
		}

		if _, err = parseSchedule(expression); err != nil {
			return nil, err
		}
	}

	state := common.Value(in.State)
	switch state {
	case "":
		state = stateEnabled
	case stateEnabled, stateDisabled:
	default:
		return nil, invalid("State must be ENABLED or DISABLED.")
	}

	if len(common.Value(in.Description)) > 512 {
		return nil, invalid("Description must be at most 512 characters.")
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if busErr := checkBus(tx, ref.scope, ref.bus); busErr != nil {
			return busErr
		}

		r, exists, loadErr := storage.Read[rule](tx, ref.key)
		if loadErr != nil {
			return loadErr
		}

		if !exists {
			if len(tx.Keys(rulePrefix(ref.scope, ref.bus))) >= maxRules {
				return awserror.New(
					"LimitExceededException",
					"The requested resource exceeds the maximum number allowed.",
				)
			}

			r = &rule{Name: *in.Name}
			if ref.bus != defaultBus {
				r.Bus = ref.bus
			}
		}

		r.Pattern, r.Schedule, r.State = text, expression, state
		r.Description, r.RoleARN = common.Value(in.Description), common.Value(in.RoleArn)
		if tagErr := mergeTags(r, tags); tagErr != nil {
			return tagErr
		}

		if scheduleErr := reschedule(tx, ref.scope, r, s.now()); scheduleErr != nil {
			return scheduleErr
		}

		return storage.Write(tx, ref.key, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.PutRuleResponse{RuleArn: new(ref.arn())}, nil
}

func (s *Service) DescribeRule(ctx context.Context, in *api.DescribeRuleRequest) (*api.DescribeRuleResponse, error) {
	ref, err := resolveRule(ctx, in.Name, in.EventBusName)
	if err != nil {
		return nil, err
	}

	var r *rule
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var loadErr error
		r, loadErr = loadRule(tx, &ref)
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	out := r.describe(ref.scope)
	return &api.DescribeRuleResponse{
		Name:               out.Name,
		Arn:                out.Arn,
		EventPattern:       out.EventPattern,
		ScheduleExpression: out.ScheduleExpression,
		State:              out.State,
		Description:        out.Description,
		RoleArn:            out.RoleArn,
		EventBusName:       out.EventBusName,
		CreatedBy:          new(ref.scope.AccountID),
	}, nil
}

// bus is the name of the rule's bus.
func (r *rule) bus() string {
	if r.Bus == "" {
		return defaultBus
	}

	return r.Bus
}

func (r *rule) describe(scope request.Scope) api.Rule {
	out := api.Rule{
		Name:         new(r.Name),
		Arn:          new(ruleARN(scope, r.bus(), r.Name)),
		State:        new(r.State),
		EventBusName: new(r.bus()),
	}
	if r.Pattern != "" {
		out.EventPattern = new(r.Pattern)
	}

	if r.Schedule != "" {
		out.ScheduleExpression = new(r.Schedule)
	}

	if r.Description != "" {
		out.Description = new(r.Description)
	}

	if r.RoleARN != "" {
		out.RoleArn = new(r.RoleARN)
	}

	return out
}

// DeleteRule succeeds for a rule that does not exist, as EventBridge does.
func (s *Service) DeleteRule(ctx context.Context, in *api.DeleteRuleRequest) (*api.DeleteRuleOutput, error) {
	ref, err := resolveRule(ctx, in.Name, in.EventBusName)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if busErr := checkBus(tx, ref.scope, ref.bus); busErr != nil {
			return busErr
		}

		r, ok, loadErr := storage.Read[rule](tx, ref.key)
		if loadErr != nil || !ok {
			return loadErr
		}

		if len(r.Targets) > 0 {
			return invalid("Rule can't be deleted since it has targets.")
		}

		tx.Delete(ref.key)
		tx.Delete(scheduleKey(ref.scope, ref.name))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteRuleOutput{}, nil
}

func (s *Service) ListRules(ctx context.Context, in *api.ListRulesRequest) (*api.ListRulesResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	bus, err := busName(m.Scope, in.EventBusName)
	if err != nil {
		return nil, err
	}

	after, size, err := page(in.NextToken, in.Limit)
	if err != nil {
		return nil, err
	}

	var rules []*rule
	prefix := rulePrefix(m.Scope, bus)
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if busErr := checkBus(tx, m.Scope, bus); busErr != nil {
			return busErr
		}

		for _, k := range tx.Keys(prefix + common.Value(in.NamePrefix)) {
			if strings.TrimPrefix(k, prefix) <= after {
				continue
			}

			r, _, loadErr := storage.Read[rule](tx, k)
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

// setState enables or disables a rule. Enabling a scheduled rule starts its
// schedule again from now; enabling an enabled rule changes nothing.
func (s *Service) setState(ctx context.Context, name, bus *string, state string) error {
	ref, err := resolveRule(ctx, name, bus)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, loadErr := loadRule(tx, &ref)
		if loadErr != nil || r.State == state {
			return loadErr
		}

		r.State = state
		if scheduleErr := reschedule(tx, ref.scope, r, s.now()); scheduleErr != nil {
			return scheduleErr
		}

		return storage.Write(tx, ref.key, r)
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
	p, err := compilePattern(common.Value(in.EventPattern))
	if err != nil {
		return nil, err
	}

	event, ok := pattern.Flatten([]byte(common.Value(in.Event)))
	if !ok {
		return nil, invalid("Parameter Event is not valid.")
	}

	return &api.TestEventPatternResponse{Result: new(p.Matches(event))}, nil
}

// eventPatterns is the dialect of EventBridge event patterns.
var eventPatterns = pattern.Dialect{MaxNumeric: 5e9, Wildcard: true}

// compilePattern compiles an event pattern, reporting errors as EventBridge does.
func compilePattern(text string) (*pattern.Pattern, error) {
	p, err := pattern.Compile(text, eventPatterns)
	if err != nil {
		return nil, awserror.New("InvalidEventPatternException", "Event pattern is not valid. Reason: "+err.Error())
	}

	return p, nil
}

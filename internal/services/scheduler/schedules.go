package scheduler

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/scheduler"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/schedule"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	modeOff        = "OFF"
	modeFlexible   = "FLEXIBLE"
	afterNone      = "NONE"
	afterDelete    = "DELETE"
	universal      = "arn:aws:scheduler:::aws-sdk:"
	maxInput       = 8192
	maxDescription = 512
	defaultPage    = 100
	atLayout       = "2006-01-02T15:04:05"
)

var (
	namePattern = regexp.MustCompile(`^[0-9a-zA-Z\-_.]{1,64}$`)
	atPattern   = regexp.MustCompile(`^at\((\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})\)$`)
	// templated are the ARN prefixes of the targets Scheduler invokes by
	// service.
	templated = []string{
		"arn:aws:lambda:", "arn:aws:sqs:", "arn:aws:sns:", "arn:aws:events:", "arn:aws:states:", "arn:aws:kinesis:",
	}
)

// plan is a parsed schedule expression: one run at a time, or a recurring
// rate or cron schedule.
type plan struct {
	at        time.Time
	recurring *schedule.Schedule
}

func parseExpression(expr, zone string) (plan, error) {
	loc := time.UTC
	if zone != "" {
		var err error
		if loc, err = time.LoadLocation(zone); err != nil {
			return plan{}, validation("Invalid ScheduleExpressionTimezone %s.", zone)
		}
	}

	if m := atPattern.FindStringSubmatch(expr); m != nil {
		at, err := time.ParseInLocation(atLayout, m[1], loc)
		if err != nil {
			return plan{}, validation("Invalid Schedule Expression %s.", expr)
		}

		return plan{at: at.UTC()}, nil
	}

	s, err := schedule.Parse(expr)
	if err != nil {
		return plan{}, validation("Invalid Schedule Expression %s: %v", expr, err)
	}

	return plan{recurring: s.In(loc)}, nil
}

// next is a schedule's first run after now, within its start and end dates.
// A one-time schedule runs once; last is its previous run, if any.
func (p plan) next(now time.Time, start, end *float64, ran bool) (time.Time, bool) {
	var t time.Time
	switch {
	case p.recurring == nil && ran:
		return time.Time{}, false
	case p.recurring == nil:
		t = p.at
	case start != nil && fromUnix(*start).After(now) && p.recurring.Period() > 0:
		t = fromUnix(*start)
	default:
		base := now
		if start != nil && fromUnix(*start).After(now) {
			base = fromUnix(*start).Add(-time.Second)
		}

		var ok bool
		if t, ok = p.recurring.First(base); !ok {
			return time.Time{}, false
		}
	}

	if end != nil && t.After(fromUnix(*end)) {
		return time.Time{}, false
	}

	return t, true
}

// settings are the members CreateSchedule and UpdateSchedule share.
type settings struct {
	Name, GroupName, ScheduleExpression, Description, ScheduleExpressionTimezone *string
	State, KmsKeyArn, ActionAfterCompletion                                      *string
	StartDate, EndDate                                                           *float64
	Target                                                                       *api.Target
	FlexibleTimeWindow                                                           *api.FlexibleTimeWindow
}

//nolint:gocyclo,cyclop // One check per member.
func check(in *settings, now time.Time) (plan, error) {
	state, after := common.Value(in.State), common.Value(in.ActionAfterCompletion)
	switch {
	case !namePattern.MatchString(common.Value(in.Name)):
		return plan{}, validation("The schedule name must be 1 to 64 letters, digits, and -_. characters.")
	case in.GroupName != nil && !namePattern.MatchString(*in.GroupName):
		return plan{}, validation("The group name must be 1 to 64 letters, digits, and -_. characters.")
	case in.ScheduleExpression == nil:
		return plan{}, validation("ScheduleExpression is required.")
	case in.State != nil && state != stateEnabled && state != stateDisabled:
		return plan{}, validation("State must be ENABLED or DISABLED.")
	case in.ActionAfterCompletion != nil && after != afterNone && after != afterDelete:
		return plan{}, validation("ActionAfterCompletion must be NONE or DELETE.")
	case len(common.Value(in.Description)) > maxDescription:
		return plan{}, validation("The description must be at most 512 characters long.")
	case in.StartDate != nil && in.EndDate != nil && *in.StartDate >= *in.EndDate:
		return plan{}, validation("The StartDate must be before the EndDate.")
	case in.EndDate != nil && fromUnix(*in.EndDate).Before(now):
		return plan{}, validation("The EndDate must be in the future.")
	}

	if err := checkWindow(in.FlexibleTimeWindow); err != nil {
		return plan{}, err
	}

	if err := checkTarget(in.Target); err != nil {
		return plan{}, err
	}

	return parseExpression(*in.ScheduleExpression, common.Value(in.ScheduleExpressionTimezone))
}

func checkWindow(w *api.FlexibleTimeWindow) error {
	if w == nil {
		return validation("FlexibleTimeWindow is required.")
	}

	switch mode, n := common.Value(w.Mode), w.MaximumWindowInMinutes; {
	case mode == modeOff && n == nil, mode == modeFlexible && n != nil && *n >= 1 && *n <= 1440:
		return nil
	case mode == modeOff:
		return validation("A FlexibleTimeWindow of OFF has no MaximumWindowInMinutes.")
	}

	return validation("A FLEXIBLE FlexibleTimeWindow needs a MaximumWindowInMinutes of 1 to 1440.")
}

//nolint:gocyclo,cyclop // One check per member.
func checkTarget(t *api.Target) error {
	if t == nil || common.Value(t.Arn) == "" || common.Value(t.RoleArn) == "" {
		return validation("A Target needs an Arn and a RoleArn.")
	}

	arn, input := *t.Arn, common.Value(t.Input)
	isUniversal := strings.HasPrefix(arn, universal)
	switch {
	case len(input) > maxInput:
		return validation("The target input must be at most 8192 characters long.")
	case !isUniversal && !hasPrefix(arn, templated):
		return validation("TinyStack does not support the target %s: use a Lambda function, SQS queue, SNS topic, "+
			"EventBridge bus, Step Functions state machine, or Kinesis stream, or a universal target.", arn)
	case isUniversal && len(strings.Split(strings.TrimPrefix(arn, universal), ":")) != 2:
		return validation("A universal target ARN is %s<service>:<apiAction>.", universal)
	case isUniversal && !json.Valid([]byte(cmpOr(input, "{}"))):
		return validation("The input of a universal target must be JSON.")
	case strings.HasPrefix(arn, "arn:aws:events:") && t.EventBridgeParameters == nil:
		return validation("An EventBridge target needs EventBridgeParameters.")
	case strings.HasPrefix(arn, "arn:aws:kinesis:") && t.KinesisParameters == nil:
		return validation("A Kinesis target needs KinesisParameters.")
	case t.DeadLetterConfig != nil && !strings.HasPrefix(common.Value(t.DeadLetterConfig.Arn), "arn:aws:sqs:"):
		return validation("The dead-letter queue must be an SQS queue ARN.")
	}

	if r := t.RetryPolicy; r != nil {
		if age := r.MaximumEventAgeInSeconds; age != nil && (*age < 60 || *age > 86400) {
			return validation("MaximumEventAgeInSeconds must be 60 to 86400.")
		}

		if n := r.MaximumRetryAttempts; n != nil && (*n < 0 || *n > 185) {
			return validation("MaximumRetryAttempts must be 0 to 185.")
		}
	}

	return nil
}

func hasPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}

	return false
}

func cmpOr(s, fallback string) string {
	if s == "" {
		return fallback
	}

	return s
}

// put stores a schedule and indexes its next run.
func (s *Service) put(tx storage.WriteTx, scope request.Scope, in *settings, p plan, created float64) (string, error) {
	now := s.now()
	groupName := cmpOr(common.Value(in.GroupName), defaultGroup)
	if _, err := loadGroup(tx, scope, groupName); err != nil {
		return "", err
	}

	name := *in.Name
	arn := scheduleARN(scope, groupName, name)
	rec := record{Schedule: api.GetScheduleOutput{
		Arn:                        new(arn),
		GroupName:                  new(groupName),
		Name:                       in.Name,
		ScheduleExpression:         in.ScheduleExpression,
		StartDate:                  in.StartDate,
		EndDate:                    in.EndDate,
		Description:                in.Description,
		ScheduleExpressionTimezone: new(cmpOr(common.Value(in.ScheduleExpressionTimezone), "UTC")),
		State:                      new(cmpOr(common.Value(in.State), stateEnabled)),
		CreationDate:               new(created),
		LastModificationDate:       new(common.Unix(now)),
		KmsKeyArn:                  in.KmsKeyArn,
		Target:                     in.Target,
		FlexibleTimeWindow:         in.FlexibleTimeWindow,
		ActionAfterCompletion: new(cmpOr(common.Value(in.ActionAfterCompletion),
			afterNone)),
	}}
	if next, ok := p.next(now, in.StartDate, in.EndDate, false); ok {
		rec.Next = common.Unix(next)
	}

	if err := storage.Write(tx, key(scope, kindSchedule, groupName+"/"+name), rec); err != nil {
		return "", err
	}

	s.index(tx, scope, &rec)
	return arn, nil
}

// index records a schedule's next run, or forgets a schedule without one.
func (s *Service) index(tx storage.WriteTx, scope request.Scope, rec *record) {
	k := indexKey(scope, common.Value(rec.Schedule.GroupName), common.Value(rec.Schedule.Name))
	if rec.Next == 0 || common.Value(rec.Schedule.State) != stateEnabled {
		tx.Delete(k)
		return
	}

	tx.Put(k, []byte("{}"))
}

func (s *Service) CreateSchedule(ctx context.Context, in *api.CreateScheduleInput) (*api.CreateScheduleOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	set := settings{
		Name: in.Name, GroupName: in.GroupName, ScheduleExpression: in.ScheduleExpression, Description: in.Description,
		ScheduleExpressionTimezone: in.ScheduleExpressionTimezone, State: in.State, KmsKeyArn: in.KmsKeyArn,
		ActionAfterCompletion: in.ActionAfterCompletion, StartDate: in.StartDate, EndDate: in.EndDate,
		Target: in.Target, FlexibleTimeWindow: in.FlexibleTimeWindow,
	}
	p, err := check(&set, s.now())
	if err != nil {
		return nil, err
	}

	var arn string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		groupName := cmpOr(common.Value(in.GroupName), defaultGroup)
		if _, exists := tx.Get(key(m.Scope, kindSchedule, groupName+"/"+*in.Name)); exists {
			return awserror.New("ConflictException", "Schedule "+*in.Name+" already exists.")
		}

		var putErr error
		arn, putErr = s.put(tx, m.Scope, &set, p, common.Unix(s.now()))
		return putErr
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateScheduleOutput{ScheduleArn: new(arn)}, nil
}

func (s *Service) UpdateSchedule(ctx context.Context, in *api.UpdateScheduleInput) (*api.UpdateScheduleOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	set := settings{
		Name: in.Name, GroupName: in.GroupName, ScheduleExpression: in.ScheduleExpression, Description: in.Description,
		ScheduleExpressionTimezone: in.ScheduleExpressionTimezone, State: in.State, KmsKeyArn: in.KmsKeyArn,
		ActionAfterCompletion: in.ActionAfterCompletion, StartDate: in.StartDate, EndDate: in.EndDate,
		Target: in.Target, FlexibleTimeWindow: in.FlexibleTimeWindow,
	}
	p, err := check(&set, s.now())
	if err != nil {
		return nil, err
	}

	var arn string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		groupName := cmpOr(common.Value(in.GroupName), defaultGroup)
		old, found, readErr := storage.Read[record](tx, key(m.Scope, kindSchedule, groupName+"/"+*in.Name))
		switch {
		case readErr != nil:
			return readErr
		case !found:
			return notFound("Schedule " + *in.Name)
		}

		var putErr error
		arn, putErr = s.put(tx, m.Scope, &set, p, common.Value(old.Schedule.CreationDate))
		return putErr
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateScheduleOutput{ScheduleArn: new(arn)}, nil
}

func (s *Service) GetSchedule(ctx context.Context, in *api.GetScheduleInput) (*api.GetScheduleOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var rec *record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var found bool
		groupName := cmpOr(common.Value(in.GroupName), defaultGroup)
		rec, found, err = storage.Read[record](tx, key(m.Scope, kindSchedule, groupName+"/"+common.Value(in.Name)))
		if err == nil && !found {
			err = notFound("Schedule " + common.Value(in.Name))
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	return &rec.Schedule, nil
}

func (s *Service) DeleteSchedule(ctx context.Context, in *api.DeleteScheduleInput) (*api.DeleteScheduleOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	groupName := cmpOr(common.Value(in.GroupName), defaultGroup)
	return &api.DeleteScheduleOutput{}, s.store.Update(ctx, func(tx storage.WriteTx) error {
		k := key(m.Scope, kindSchedule, groupName+"/"+common.Value(in.Name))
		if _, found := tx.Get(k); !found {
			return notFound("Schedule " + common.Value(in.Name))
		}

		tx.Delete(k)
		tx.Delete(indexKey(m.Scope, groupName, common.Value(in.Name)))
		return nil
	})
}

func (s *Service) ListSchedules(ctx context.Context, in *api.ListSchedulesInput) (*api.ListSchedulesOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	limit := int64(defaultPage)
	if in.MaxResults != nil {
		limit = *in.MaxResults
	}

	if limit < 1 || limit > defaultPage {
		return nil, validation("MaxResults must be 1 to 100.")
	}

	out := &api.ListSchedulesOutput{Schedules: []api.ScheduleSummary{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		p := key(m.Scope, kindSchedule, "")
		if in.GroupName != nil {
			if _, groupErr := loadGroup(tx, m.Scope, *in.GroupName); groupErr != nil {
				return groupErr
			}

			p += *in.GroupName + "/"
		}

		var last string
		for _, k := range tx.Keys(p) {
			id := strings.TrimPrefix(k, key(m.Scope, kindSchedule, ""))
			if in.NextToken != nil && id <= *in.NextToken {
				continue
			}

			rec, _, readErr := storage.Read[record](tx, k)
			if readErr != nil {
				return readErr
			}

			sc := &rec.Schedule
			if !strings.HasPrefix(common.Value(sc.Name), common.Value(in.NamePrefix)) ||
				in.State != nil && common.Value(sc.State) != *in.State {
				continue
			}

			if int64(len(out.Schedules)) == limit {
				out.NextToken = new(last)
				return nil
			}

			out.Schedules = append(out.Schedules, api.ScheduleSummary{
				Arn: sc.Arn, Name: sc.Name, GroupName: sc.GroupName, State: sc.State, CreationDate: sc.CreationDate,
				LastModificationDate: sc.LastModificationDate, Target: &api.TargetSummary{Arn: sc.Target.Arn},
			})
			last = id
		}

		return nil
	})

	return out, err
}

package autoscaling

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/autoscaling"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/schedule"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var number = regexp.MustCompile(`\d+`)

// scheduled is a scheduled action's record. Next is its next run, or zero
// when it has none.
type scheduled struct {
	Next   time.Time
	Action api.ScheduledUpdateGroupAction
}

// recurrence parses a Unix cron expression (minute, hour, day of month,
// month, and day of week from 0, Sunday) as the AWS cron expression it
// means.
func recurrence(expr, zone string) (*schedule.Schedule, error) {
	invalid := validation("Given recurrence %q is not a valid cron expression.", expr)
	fields := strings.Fields(expr)
	if len(fields) != 5 || strings.Contains(expr, "?") {
		return nil, invalid
	}

	dom, dow := fields[2], fields[4]
	switch {
	case dow == "*":
		dow = "?"
	case dom == "*":
		dom = "?"
	default:
		return nil, validation("A recurrence cannot set both the day of the month and the day of the week.")
	}

	if dow != "?" {
		parts := strings.Split(dow, ",")
		for i, part := range parts {
			days, step, stepped := strings.Cut(part, "/")
			days = number.ReplaceAllStringFunc(days, func(n string) string {
				d, _ := strconv.Atoi(n)
				return strconv.Itoa(d%7 + 1)
			})
			if stepped {
				days += "/" + step
			}

			parts[i] = days
		}

		dow = strings.Join(parts, ",")
	}

	s, err := schedule.Parse(fmt.Sprintf("cron(%s %s %s %s %s *)", fields[0], fields[1], dom, fields[3], dow))
	if err != nil {
		return nil, invalid
	}

	loc := time.UTC
	if zone != "" {
		if loc, err = time.LoadLocation(zone); err != nil {
			return nil, validation("Given time zone %q is not valid.", zone)
		}
	}

	return s.In(loc), nil
}

// next is an action's first run after now.
func (a *scheduled) next(now time.Time) error {
	act := &a.Action
	a.Next = time.Time{}
	if act.Recurrence == nil {
		if act.Time != nil && act.Time.After(now) {
			a.Next = *act.Time
		}

		return nil
	}

	s, err := recurrence(*act.Recurrence, common.Value(act.TimeZone))
	if err != nil {
		return err
	}

	base := now
	if act.StartTime != nil && act.StartTime.After(now) {
		base = act.StartTime.Add(-time.Second)
	}

	next, ok := s.First(base)
	if ok && (act.EndTime == nil || !next.After(*act.EndTime)) {
		a.Next = next
	}

	return nil
}

// PutScheduledUpdateGroupAction creates or replaces a scheduled action: a
// one-time action at Time (StartTime), or a recurring one.
func (s *Service) PutScheduledUpdateGroupAction(
	ctx context.Context, in *api.PutScheduledUpdateGroupActionType,
) (*api.PutScheduledUpdateGroupActionOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	groupName, name, now := common.Value(in.AutoScalingGroupName), common.Value(in.ScheduledActionName), s.now().UTC()
	if checkErr := checkName("ScheduledActionName", name); checkErr != nil {
		return nil, checkErr
	}

	when := in.Time
	if when == nil && in.Recurrence == nil {
		when = in.StartTime
	}

	switch {
	case in.MinSize == nil && in.MaxSize == nil && in.DesiredCapacity == nil:
		return nil, validation("At least one of MinSize, MaxSize, and DesiredCapacity must be specified.")
	case in.Time != nil && in.Recurrence != nil:
		return nil, validation("Time and Recurrence cannot both be specified.")
	case when == nil && in.Recurrence == nil:
		return nil, validation("A scheduled action needs a Time, StartTime, or Recurrence.")
	case when != nil && when.Before(now):
		return nil, validation("Given start time is in the past")
	case in.StartTime != nil && in.EndTime != nil && !in.EndTime.After(*in.StartTime):
		return nil, validation("The EndTime must be after the StartTime.")
	}

	a := &scheduled{Action: api.ScheduledUpdateGroupAction{
		AutoScalingGroupName: in.AutoScalingGroupName, ScheduledActionName: in.ScheduledActionName,
		ScheduledActionARN: new(arn(m.Scope, "scheduledUpdateGroupAction:"+newID()+":autoScalingGroupName/"+
			groupName+":scheduledActionName/"+name)),
		Time: when, StartTime: in.StartTime, EndTime: in.EndTime, Recurrence: in.Recurrence,
		MinSize: in.MinSize, MaxSize: in.MaxSize, DesiredCapacity: in.DesiredCapacity, TimeZone: in.TimeZone,
	}}
	if a.Action.StartTime == nil {
		a.Action.StartTime = when
	}

	if checkErr := a.next(now); checkErr != nil {
		return nil, checkErr
	}

	if a.Action.Recurrence != nil && a.Action.StartTime == nil {
		a.Action.StartTime = new(a.Next)
	}

	return &api.PutScheduledUpdateGroupActionOutput{}, s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, loadErr := loadGroup(tx, m.Scope, groupName); loadErr != nil {
			return loadErr
		}

		k := key(m.Scope, kindScheduled, groupName+"/"+name)
		if old, found, readErr := storage.Read[scheduled](tx, k); readErr != nil {
			return readErr
		} else if found {
			a.Action.ScheduledActionARN = old.Action.ScheduledActionARN
		}

		return storage.Write(tx, k, a)
	})
}

func (s *Service) DescribeScheduledActions(
	ctx context.Context, in *api.DescribeScheduledActionsType,
) (*api.ScheduledActionsType, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var all []*scheduled
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		p := key(m.Scope, kindScheduled, "")
		if in.AutoScalingGroupName != nil {
			if _, loadErr := loadGroup(tx, m.Scope, *in.AutoScalingGroupName); loadErr != nil {
				return loadErr
			}

			p += *in.AutoScalingGroupName + "/"
		}

		for _, k := range tx.Keys(p) {
			a, _, readErr := storage.Read[scheduled](tx, k)
			if readErr != nil {
				return readErr
			}

			switch {
			case len(in.ScheduledActionNames) > 0 &&
				!slices.Contains(in.ScheduledActionNames, common.Value(a.Action.ScheduledActionName)):
			case in.StartTime != nil && a.Next.Before(*in.StartTime):
			case in.EndTime != nil && a.Next.After(*in.EndTime):
			default:
				all = append(all, a)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	id := func(a *scheduled) string {
		return common.Value(a.Action.AutoScalingGroupName) + "/" + common.Value(a.Action.ScheduledActionName)
	}
	page, next, err := window(all, id, in.NextToken, in.MaxRecords)
	if err != nil {
		return nil, err
	}

	out := &api.ScheduledActionsType{ScheduledUpdateGroupActions: []api.ScheduledUpdateGroupAction{}, NextToken: next}
	for _, a := range page {
		action := a.Action
		if action.Recurrence != nil && !a.Next.IsZero() {
			action.StartTime = new(a.Next)
		}

		out.ScheduledUpdateGroupActions = append(out.ScheduledUpdateGroupActions, action)
	}

	return out, nil
}

func (s *Service) DeleteScheduledAction(
	ctx context.Context, in *api.DeleteScheduledActionType,
) (*api.DeleteScheduledActionOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	groupName, name := common.Value(in.AutoScalingGroupName), common.Value(in.ScheduledActionName)
	return &api.DeleteScheduledActionOutput{}, s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, loadErr := loadGroup(tx, m.Scope, groupName); loadErr != nil {
			return loadErr
		}

		k := key(m.Scope, kindScheduled, groupName+"/"+name)
		if _, found := tx.Get(k); !found {
			return validation("Scheduled action name not found - %s", name)
		}

		tx.Delete(k)
		return nil
	})
}

// runScheduled applies a group's due scheduled actions in one transaction,
// advancing recurring ones and deleting the rest.
func (s *Service) runScheduled(ctx context.Context, scope request.Scope, name string) error {
	now := s.now().UTC()
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		g, found, err := storage.Read[group](tx, key(scope, kindGroup, name))
		if err != nil || !found || g.suspended(processScheduled) {
			return err
		}

		changed := false
		for _, k := range tx.Keys(key(scope, kindScheduled, name+"/")) {
			a, _, readErr := storage.Read[scheduled](tx, k)
			if readErr != nil {
				return readErr
			}

			if a.Next.IsZero() || a.Next.After(now) {
				continue
			}

			act := &a.Action
			from := common.Value(g.Group.DesiredCapacity)
			out := &g.Group
			for _, set := range []struct {
				from *int64
				to   **int64
			}{{act.MinSize, &out.MinSize}, {act.MaxSize, &out.MaxSize}, {act.DesiredCapacity, &out.DesiredCapacity}} {
				if set.from != nil {
					*set.to = new(*set.from)
				}
			}

			desired := min(max(common.Value(out.DesiredCapacity), common.Value(out.MinSize)), common.Value(out.MaxSize))
			out.DesiredCapacity = new(desired)
			activity := s.activity(g, now, "Scheduled action "+common.Value(act.ScheduledActionName), fmt.Sprintf(
				"At %s a scheduled action update of AutoScalingGroup constraints to min: %d, max: %d, desired: %d "+
					"changing the desired capacity from %d to %d.", stamp(now), common.Value(out.MinSize), common.Value(out.MaxSize),
				desired, from, desired), nil)
			if writeErr := storage.Write(
				tx,
				key(scope, kindActivity, name+"/"+s.sequenced(now)),
				activity,
			); writeErr != nil {
				return writeErr
			}

			changed = true
			if act.Recurrence == nil {
				tx.Delete(k)
				continue
			}

			if checkErr := a.next(now); checkErr != nil {
				return checkErr
			}

			if writeErr := storage.Write(tx, k, a); writeErr != nil {
				return writeErr
			}
		}

		if !changed {
			return nil
		}

		return storage.Write(tx, key(scope, kindGroup, name), g)
	})
}

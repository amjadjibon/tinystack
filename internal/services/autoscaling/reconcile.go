package autoscaling

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/autoscaling"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var (
	// runInterval is how often the runner visits every group.
	runInterval = time.Second
	// retryAfter is how long a group waits to launch again after a launch
	// failed.
	retryAfter = 10 * time.Second
)

func (s *Service) visit(ctx context.Context) {
	type entry struct {
		scope request.Scope
		name  string
	}

	var groups []entry
	_ = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(indexPrefix) {
			parts := strings.SplitN(strings.TrimPrefix(k, indexPrefix), "/", 3)
			if len(parts) == 3 {
				groups = append(groups, entry{request.Scope{AccountID: parts[0], Region: parts[1]}, parts[2]})
			}
		}

		return nil
	})

	for _, g := range groups {
		if ctx.Err() != nil {
			return
		}

		if err := s.runScheduled(ctx, g.scope, g.name); err != nil {
			s.logger.Warn("running scheduled actions failed", "group", g.name, "error", err)
		}

		s.reconcile(ctx, g.scope, g.name, "")
	}
}

func memberIDs(members []member) []string {
	ids := make([]string, len(members))
	for i, m := range members {
		ids[i] = common.Value(m.InstanceId)
	}

	return ids
}

// departure is a member leaving its group, and why.
type departure struct {
	member
	cause string
}

// reconcile brings a group to its desired capacity: it takes out members
// that are unhealthy or that EC2 no longer runs, terminates the members a
// smaller capacity leaves over (by the group's termination policy), and
// launches instances into the zones with the fewest members. cause, when not
// empty, starts the cause of each activity. It does nothing before app.New
// wires in the caller.
func (s *Service) reconcile(ctx context.Context, scope request.Scope, name, cause string) {
	if s.caller == nil {
		return
	}

	s.busy.Lock()
	defer s.busy.Unlock()
	now := s.now().UTC()
	var g *group
	var lc *api.LaunchConfiguration
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var err error
		if g, _, err = storage.Read[group](tx, scope.Key(service, kindGroup, name)); err != nil || g == nil {
			return err
		}

		if n := g.Group.LaunchConfigurationName; n != nil {
			lc, _, err = storage.Read[api.LaunchConfiguration](tx, scope.Key(service, kindConfiguration, *n))
		}

		return err
	})
	if err != nil || g == nil {
		if err != nil {
			s.logger.Warn("reading an Auto Scaling group failed", "group", name, "error", err)
		}

		return
	}

	unhealthy := map[string]bool{}
	if len(g.Members) > 0 && !g.suspended(processHealthCheck) {
		up, runErr := s.running(ctx, scope, memberIDs(g.Members))
		if runErr != nil {
			s.logger.Warn("checking an Auto Scaling group's instances failed", "group", name, "error", runErr)
			return
		}

		for _, m := range g.Members {
			if !up[common.Value(m.InstanceId)] {
				unhealthy[common.Value(m.InstanceId)] = true
			}
		}
	}

	var gone []departure
	var kept []member
	for _, m := range g.Members {
		id := common.Value(m.InstanceId)
		sick := unhealthy[id] || common.Value(m.HealthStatus) == healthUnhealthy
		if sick && !g.suspended(processReplaceUnhealthy) {
			gone = append(gone, departure{m, fmt.Sprintf("At %s an instance was taken out of service in response "+
				"to an EC2 health check indicating it has been terminated or stopped.", stamp(now))})
			continue
		}

		kept = append(kept, m)
	}

	desired := int(common.Value(g.Group.DesiredCapacity))
	if extra := len(kept) - desired; extra > 0 && !g.suspended(processTerminate) {
		for _, v := range victims(kept, extra, g.Group.TerminationPolicies) {
			size := len(kept)
			kept = slices.DeleteFunc(
				kept,
				func(m member) bool { return common.Value(m.InstanceId) == common.Value(v.InstanceId) },
			)
			gone = append(gone, departure{v, joinCause(cause, fmt.Sprintf("At %s an instance was taken out of "+
				"service in response to a difference between desired and actual capacity, shrinking the capacity "+
				"from %d to %d.", stamp(now), size, size-1))})
		}
	}

	var launched []member
	var failure error
	if missing := desired - len(kept); missing > 0 && !g.suspended(processLaunch) && !now.Before(g.Retry) {
		for range missing {
			subnet, zone := g.place(append(slices.Clone(kept), launched...))
			m, launchErr := s.launch(ctx, scope, g, lc, subnet, zone)
			if launchErr != nil {
				failure = launchErr
				break
			}

			launched = append(launched, m)
		}
	}

	departed := make([]member, len(gone))
	for i := range gone {
		departed[i] = gone[i].member
	}

	s.terminate(ctx, scope, memberIDs(departed))
	if len(gone) == 0 && len(launched) == 0 && failure == nil && len(unhealthy) == 0 {
		return
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		cur, found, readErr := storage.Read[group](tx, scope.Key(service, kindGroup, name))
		if readErr != nil || !found {
			return readErr
		}

		leaving := map[string]bool{}
		for i := range gone {
			d := &gone[i]
			leaving[common.Value(d.InstanceId)] = true
			a := s.activity(cur, now, "Terminating EC2 instance: "+common.Value(d.InstanceId), d.cause, &d.member)
			if writeErr := storage.Write(
				tx,
				scope.Key(service, kindActivity, name+"/"+s.sequenced(now)),
				a,
			); writeErr != nil {
				return writeErr
			}
		}

		cur.Members = slices.DeleteFunc(cur.Members, func(m member) bool { return leaving[common.Value(m.InstanceId)] })
		for i := range cur.Members {
			if unhealthy[common.Value(cur.Members[i].InstanceId)] {
				cur.Members[i].HealthStatus = new(healthUnhealthy)
			}
		}

		for i, m := range launched {
			size := len(cur.Members)
			a := s.activity(cur, now, "Launching a new EC2 instance: "+common.Value(m.InstanceId),
				joinCause(cause, fmt.Sprintf("At %s an instance was started in response to a difference between "+
					"desired and actual capacity, increasing the capacity from %d to %d.", stamp(now), size, size+1)), &m)
			if writeErr := storage.Write(
				tx,
				scope.Key(service, kindActivity, name+"/"+s.sequenced(now.Add(time.Duration(i)))),
				a,
			); writeErr != nil {
				return writeErr
			}

			cur.Members = append(cur.Members, m)
		}

		if failure != nil {
			cur.Retry = now.Add(retryAfter)
			a := s.activity(cur, now, "Launching a new EC2 instance.  Status Reason: "+awserror.From(failure).Message,
				joinCause(cause, fmt.Sprintf("At %s an instance was started in response to a difference between "+
					"desired and actual capacity, increasing the capacity from %d to %d.", stamp(now),
					len(cur.Members), len(cur.Members)+1)), nil)
			a.StatusCode, a.StatusMessage = new("Failed"), new(awserror.From(failure).Message)
			if writeErr := storage.Write(
				tx,
				scope.Key(service, kindActivity, name+"/"+s.sequenced(now)),
				a,
			); writeErr != nil {
				return writeErr
			}
		}

		return storage.Write(tx, scope.Key(service, kindGroup, name), cur)
	})
	if err != nil {
		s.logger.Warn("saving an Auto Scaling group's instances failed", "group", name, "error", err)
	}
}

func joinCause(first, then string) string {
	if first == "" {
		return then
	}

	return first + ".  " + then
}

// place returns the subnet (or, for a group without subnets, the zone) the
// next instance launches into: the zone with the fewest members, then the
// subnet with the fewest.
func (g *group) place(members []member) (subnet, zone string) {
	count := func(match func(member) bool) int {
		n := 0
		for _, m := range members {
			if match(m) {
				n++
			}
		}

		return n
	}

	zone = slices.MinFunc(g.Group.AvailabilityZones, func(a, b string) int {
		return cmp.Or(cmp.Compare(count(func(m member) bool { return common.Value(m.AvailabilityZone) == a }),
			count(func(m member) bool { return common.Value(m.AvailabilityZone) == b })), strings.Compare(a, b))
	})

	var candidates []string
	for id, z := range g.Subnets {
		if z == zone {
			candidates = append(candidates, id)
		}
	}

	if len(candidates) == 0 {
		return "", zone
	}

	return slices.MinFunc(candidates, func(a, b string) int {
		return cmp.Or(cmp.Compare(count(func(m member) bool { return m.Subnet == a }),
			count(func(m member) bool { return m.Subnet == b })), strings.Compare(a, b))
	}), zone
}

// victims chooses n unprotected members to terminate: from the zone with the
// most members, then by the first termination policy TinyStack orders by
// (OldestInstance or NewestInstance), and otherwise the oldest.
func victims(members []member, n int, policies []string) []member {
	candidates := slices.DeleteFunc(
		slices.Clone(members),
		func(m member) bool { return common.Value(m.ProtectedFromScaleIn) },
	)
	newest := slices.Index(policies, "NewestInstance") >= 0 &&
		(slices.Index(policies, "OldestInstance") < 0 ||
			slices.Index(policies, "NewestInstance") < slices.Index(policies, "OldestInstance"))
	var out []member
	for range min(n, len(candidates)) {
		perZone := map[string]int{}
		for _, m := range members {
			perZone[common.Value(m.AvailabilityZone)]++
		}

		for _, m := range out {
			perZone[common.Value(m.AvailabilityZone)]--
		}

		best := slices.MinFunc(candidates, func(a, b member) int {
			byZone := cmp.Compare(perZone[common.Value(b.AvailabilityZone)], perZone[common.Value(a.AvailabilityZone)])
			byAge := a.Launched.Compare(b.Launched)
			if newest {
				byAge = -byAge
			}

			return cmp.Or(byZone, byAge, strings.Compare(common.Value(a.InstanceId), common.Value(b.InstanceId)))
		})
		out = append(out, best)
		candidates = slices.DeleteFunc(
			candidates,
			func(m member) bool { return common.Value(m.InstanceId) == common.Value(best.InstanceId) },
		)
	}

	return out
}

// activity returns a finished, successful scaling activity of a group.
func (s *Service) activity(g *group, now time.Time, description, cause string, m *member) *api.Activity {
	details := "{}"
	if m != nil {
		detail := map[string]string{"Availability Zone": common.Value(m.AvailabilityZone)}
		if m.Subnet != "" {
			detail["Subnet ID"] = m.Subnet
		}

		data, _ := json.Marshal(detail)
		details = string(data)
	}

	return &api.Activity{
		ActivityId: new(newID()), AutoScalingGroupName: g.Group.AutoScalingGroupName,
		AutoScalingGroupARN: g.Group.AutoScalingGroupARN, Description: new(description), Cause: new(cause),
		StartTime: new(now), EndTime: new(now), StatusCode: new("Successful"), Progress: new(int64(100)),
		Details: new(details),
	}
}

// invert maps a key to one that sorts in the opposite order, for listing
// activities newest first.
func invert(k string) string {
	b := []byte(k)
	for i, c := range b {
		if c >= '0' && c <= '9' {
			b[i] = '9' - c + '0'
		}
	}

	return string(b)
}

func (s *Service) DescribeScalingActivities(
	ctx context.Context, in *api.DescribeScalingActivitiesType,
) (*api.ActivitiesType, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	type entry struct {
		activity *api.Activity
		key      string
	}

	var all []entry
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		p := m.Key(service, kindActivity, "")
		if in.AutoScalingGroupName != nil {
			if _, loadErr := loadGroup(tx, m.Scope, *in.AutoScalingGroupName); loadErr != nil {
				return loadErr
			}

			p += *in.AutoScalingGroupName + "/"
		}

		for _, k := range tx.Keys(p) {
			a, _, readErr := storage.Read[api.Activity](tx, k)
			if readErr != nil {
				return readErr
			}

			if len(in.ActivityIds) == 0 || slices.Contains(in.ActivityIds, common.Value(a.ActivityId)) {
				seq := k[strings.LastIndexByte(k, '/')+1:]
				all = append(all, entry{a, invert(seq) + common.Value(a.ActivityId)})
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	slices.SortFunc(all, func(a, b entry) int { return strings.Compare(a.key, b.key) })
	page, next, err := window(all, func(e entry) string { return e.key }, in.NextToken, in.MaxRecords)
	if err != nil {
		return nil, err
	}

	out := &api.ActivitiesType{Activities: []api.Activity{}, NextToken: next}
	for _, e := range page {
		out.Activities = append(out.Activities, *e.activity)
	}

	return out, nil
}

package batch

import (
	"cmp"
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/common"
)

func jobFIFO(a, b *record) int {
	return cmp.Or(cmp.Compare(common.Value(a.Job.CreatedAt), common.Value(b.Job.CreatedAt)),
		strings.Compare(a.ID, b.ID))
}

type fairQueue struct {
	policy    *record
	shares    map[string][]*record
	usage     map[string]float64
	active    map[string]map[string]bool
	positions []int
}

func (q *fairQueue) weight(share string) float64 {
	for _, rule := range q.policy.Fairshare.ShareDistribution {
		pattern := common.Value(rule.ShareIdentifier)
		if share == pattern ||
			strings.HasSuffix(pattern, "*") && strings.HasPrefix(share, strings.TrimSuffix(pattern, "*")) {
			return common.Value(common.OrElse(rule.WeightFactor, new(1.0)))
		}
	}

	return 1
}

func (q *fairQueue) claim(environment, share string) {
	if q.active[environment] == nil {
		q.active[environment] = map[string]bool{}
	}

	q.active[environment][share] = true
}

func (q *fairQueue) permits(environment, share string, used, cost, capacity int64) bool {
	shares := q.active[environment]
	if !shares[share] {
		return true
	}

	fraction := float64(common.Value(q.policy.Fairshare.ComputeReservation)) / 100
	reserved := math.Pow(fraction, float64(len(shares))) * float64(capacity)
	return float64(used+cost) <= float64(capacity)-reserved
}

func (q *fairQueue) track(r *record, index int, now int64) {
	share := common.Value(r.Job.ShareIdentifier)
	cpu := float64(jobCPUUnits(r.Job.Container))
	status := common.Value(r.Job.Status)
	if status == statusRunnable && !r.Cancelled {
		q.positions = append(q.positions, index)
		q.shares[share] = append(q.shares[share], r)
	}

	if status == statusStarting || status == statusRunning {
		q.usage[share] += cpu
		q.claim(r.ComputeEnvironment, share)
	}

	seconds := common.Value(q.policy.Fairshare.ShareDecaySeconds)
	if seconds == 0 {
		seconds = 600
	}

	window := seconds * 1000
	for _, attempt := range r.Job.Attempts {
		started, stopped := common.Value(attempt.StartedAt), common.Value(attempt.StoppedAt)
		if started != 0 && stopped > now-window {
			// Count the fraction of the trailing window during which the attempt ran.
			duration := max(0, min(now, stopped)-max(now-window, started))
			q.usage[share] += cpu * float64(duration) / float64(window)
		}
	}
}

func orderJobs(all []*record, queues, policies map[string]*record, now int64) map[string]*fairQueue {
	slices.SortFunc(all, func(a, b *record) int {
		return cmp.Or(cmp.Compare(queuePriority(queues[common.Value(b.Job.JobQueue)]),
			queuePriority(queues[common.Value(a.Job.JobQueue)])), jobFIFO(a, b))
	})
	fair := map[string]*fairQueue{}
	for arn, q := range queues {
		policy := policies[common.Value(q.Queue.SchedulingPolicyArn)]
		if policy != nil && policy.Fairshare != nil {
			fair[arn] = &fairQueue{
				policy: policy, shares: map[string][]*record{}, usage: map[string]float64{},
				active: map[string]map[string]bool{},
			}
		}
	}

	for i, r := range all {
		q := fair[common.Value(r.Job.JobQueue)]
		if q != nil && (r.Job.ArrayProperties == nil || r.Parent != "") {
			q.track(r, i, now)
		}
	}

	for _, q := range fair {
		q.reorder(all)
	}

	return fair
}

func queuePriority(q *record) int64 {
	if q == nil {
		return 0
	}

	return common.Value(q.Queue.Priority)
}

// Project cumulative demand inside each share, in priority order. Then merge shares
// by projected weighted usage while keeping the queue's slots in the global order.
func (q *fairQueue) reorder(all []*record) {
	type candidate struct {
		job   *record
		score float64
	}

	ranked := make([]candidate, 0, len(q.positions))
	for _, share := range slices.Sorted(maps.Keys(q.shares)) {
		jobs := q.shares[share]
		slices.SortFunc(jobs, func(a, b *record) int {
			return cmp.Or(cmp.Compare(common.Value(b.Job.SchedulingPriority), common.Value(a.Job.SchedulingPriority)),
				jobFIFO(a, b))
		})
		weight, usage := q.weight(share), q.usage[share]
		for _, job := range jobs {
			usage += float64(jobCPUUnits(job.Job.Container))
			ranked = append(ranked, candidate{job, weight * usage})
		}
	}

	slices.SortFunc(ranked, func(a, b candidate) int {
		return cmp.Or(cmp.Compare(a.score, b.score), jobFIFO(a.job, b.job))
	})
	for i, index := range q.positions {
		all[index] = ranked[i].job
	}
}

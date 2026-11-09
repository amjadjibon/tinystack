package xray

import (
	"context"
	"sort"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/xray"
	"github.com/amjadjibon/tinystack/internal/aws/request"
)

type graphNode struct {
	service api.Service
	edges   map[string]*graphEdge
}

type graphEdge struct {
	stats *api.EdgeStatistics
	start float64
	end   float64
}

func (n *graphNode) addEdge(key string, d *segmentDocument) {
	edge := n.edges[key]
	if edge == nil {
		v := emptyStats()
		edge = &graphEdge{
			stats: &api.EdgeStatistics{
				OkCount:           v.OkCount,
				TotalCount:        v.TotalCount,
				TotalResponseTime: v.TotalResponseTime,
				ErrorStatistics:   v.ErrorStatistics,
				FaultStatistics:   v.FaultStatistics,
			},
			start: d.Start, end: d.end(),
		}
		n.edges[key] = edge
	}

	if d.Start < edge.start {
		edge.start = d.Start
	}

	if d.end() > edge.end {
		edge.end = d.end()
	}

	v := edge.stats
	addStats(&api.ServiceStatistics{
		OkCount: v.OkCount, TotalCount: v.TotalCount,
		TotalResponseTime: v.TotalResponseTime, ErrorStatistics: v.ErrorStatistics, FaultStatistics: v.FaultStatistics,
	}, d)
}

func serviceType(d *segmentDocument) string {
	if d.Origin != "" {
		return d.Origin
	}

	if d.Namespace == awsNamespace {
		return "AWS::" + strings.TrimPrefix(d.Name, "AWS::")
	}

	return d.Namespace
}

func emptyStats() *api.ServiceStatistics {
	return &api.ServiceStatistics{
		OkCount:           new(int64(0)),
		TotalCount:        new(int64(0)),
		TotalResponseTime: new(float64(0)),
		ErrorStatistics: &api.ErrorStatistics{
			ThrottleCount: new(int64(0)),
			OtherCount:    new(int64(0)),
			TotalCount:    new(int64(0)),
		},
		FaultStatistics: &api.FaultStatistics{OtherCount: new(int64(0)), TotalCount: new(int64(0))},
	}
}

func addStats(stats *api.ServiceStatistics, d *segmentDocument) {
	if d.InProgress {
		return
	}

	*stats.TotalCount++
	*stats.TotalResponseTime += d.end() - d.Start
	switch {
	case d.Throttle || d.status() == 429:
		*stats.ErrorStatistics.ThrottleCount++
		*stats.ErrorStatistics.TotalCount++
	case d.Fault || d.status() >= 500:
		*stats.FaultStatistics.OtherCount++
		*stats.FaultStatistics.TotalCount++
	case d.Error || d.status() >= 400:
		*stats.ErrorStatistics.OtherCount++
		*stats.ErrorStatistics.TotalCount++
	default:
		*stats.OkCount++
	}
}

func buildGraph(records []traceRecord, account string) []api.Service {
	nodes := map[string]*graphNode{}
	nodeKey := func(d *segmentDocument) string { return d.Name + "\x00" + serviceType(d) }
	var visit func(*segmentDocument, string, bool)
	visit = func(d *segmentDocument, parent string, root bool) {
		key := parent
		if parent == "" || d.Namespace == awsNamespace || d.Namespace == "remote" || root {
			key = nodeKey(d)
			n := nodes[key]
			if n == nil {
				n = &graphNode{
					service: api.Service{
						Name:              new(d.Name),
						Names:             []string{d.Name},
						Type:              new(serviceType(d)),
						AccountId:         new(account),
						Root:              new(false),
						StartTime:         new(d.Start),
						EndTime:           new(d.end()),
						SummaryStatistics: emptyStats(),
					},
					edges: map[string]*graphEdge{},
				}

				nodes[key] = n
			}

			*n.service.Root = *n.service.Root || root
			if d.Start < *n.service.StartTime {
				*n.service.StartTime = d.Start
			}

			if d.end() > *n.service.EndTime {
				*n.service.EndTime = d.end()
			}

			addStats(n.service.SummaryStatistics, d)
			if parent != "" && parent != key {
				nodes[parent].addEdge(key, d)
			}
		}

		for _, sub := range d.Subsegments {
			visit(sub, key, false)
		}
	}

	for _, r := range records {
		docs := r.documents()
		byID := map[string]*segmentDocument{}
		for _, d := range docs {
			byID[d.ID] = d
		}

		ids := make([]string, 0, len(r.Segments))
		for id := range r.Segments {
			ids = append(ids, id)
		}

		sort.Strings(ids)
		// Establish services before linking independently uploaded child segments.
		for _, id := range ids {
			d := r.Segments[id].Segment
			if d.Type != subsegmentType {
				copyDoc := *d
				copyDoc.Subsegments = nil
				visit(&copyDoc, "", d.ParentID == "")
			}
		}

		for _, id := range ids {
			d := r.Segments[id].Segment
			parent := ""
			if p, ok := byID[d.ParentID]; ok {
				parent = nodeKey(p)
				if nodes[parent] == nil {
					parent = ""
				}
			}

			if d.Type == subsegmentType {
				visit(d, parent, false)
				continue
			}

			key := nodeKey(d)
			if parent != "" && parent != key {
				nodes[parent].addEdge(key, d)
			}

			for _, sub := range d.Subsegments {
				visit(sub, key, false)
			}
		}
	}

	keys := make([]string, 0, len(nodes))
	for k := range nodes {
		keys = append(keys, k)
	}

	sort.Strings(keys)
	refs := map[string]int64{}
	for i, k := range keys {
		refs[k] = int64(i + 1)
	}

	out := make([]api.Service, 0, len(keys))
	for _, key := range keys {
		n := nodes[key]
		n.service.ReferenceId = new(refs[key])
		targets := make([]string, 0, len(n.edges))
		for target := range n.edges {
			targets = append(targets, target)
		}

		sort.Strings(targets)
		for _, target := range targets {
			n.service.Edges = append(
				n.service.Edges,
				api.Edge{
					ReferenceId:       new(refs[target]),
					StartTime:         new(n.edges[target].start),
					EndTime:           new(n.edges[target].end),
					SummaryStatistics: n.edges[target].stats,
				},
			)
		}

		out = append(out, n.service)
	}

	return out
}

func (s *Service) GetTraceGraph(ctx context.Context, in *api.GetTraceGraphRequest) (*api.GetTraceGraphResult, error) {
	if in == nil {
		return nil, invalid("TraceIds required")
	}

	if err := checkTraceIDs(in.TraceIds, 5); err != nil {
		return nil, err
	}

	records, err := s.traces(ctx)
	if err != nil {
		return nil, err
	}

	ids := map[string]bool{}
	for _, id := range in.TraceIds {
		ids[id] = true
	}

	selected := []traceRecord{}
	for _, r := range records {
		if ids[r.ID] {
			selected = append(selected, r)
		}
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	return &api.GetTraceGraphResult{Services: buildGraph(selected, scope.AccountID)}, nil
}

func (s *Service) GetServiceGraph(
	ctx context.Context,
	in *api.GetServiceGraphRequest,
) (*api.GetServiceGraphResult, error) {
	if in == nil {
		return nil, invalid("StartTime and EndTime required")
	}

	if err := checkTimeRange(in.StartTime, in.EndTime); err != nil {
		return nil, err
	}

	records, err := s.traces(ctx)
	if err != nil {
		return nil, err
	}

	selected := []traceRecord{}
	for _, r := range records {
		if traceInRange(r, *in.StartTime, *in.EndTime, "Event") {
			selected = append(selected, r)
		}
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	return &api.GetServiceGraphResult{
		StartTime: in.StartTime,
		EndTime:   in.EndTime,
		Services:  buildGraph(selected, scope.AccountID),
	}, nil
}

package datasync

import (
	"context"
	"math"
	"strconv"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/datasync"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func checkFilter(name, op string, values []string, task bool) error {
	if name == "CreationTime" {
		switch op {
		case operatorEquals, operatorNotEquals, "LessThan", "LessThanOrEqual", "GreaterThan", "GreaterThanOrEqual":
		default:
			return invalid("Operator %s is unsupported for CreationTime", op)
		}

		for _, v := range values {
			if _, err := filterTime(v); err != nil {
				return err
			}
		}

		return nil
	}

	if task && name != "LocationId" || !task && name != "LocationUri" && name != "LocationType" {
		return invalid("Unsupported filter name %s", name)
	}

	switch op {
	case operatorEquals, operatorNotEquals, "In", "Contains", operatorNotContains, "BeginsWith":
		return nil
	default:
		return invalid("Unsupported operator %s for %s", op, name)
	}
}

func filterTime(v string) (float64, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02:15:04:05"} {
		t, err := time.Parse(layout, v)
		if err == nil {
			return float64(t.UnixMilli()) / 1000, nil
		}
	}

	n, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, invalid(
			"CreationTime filter requires a UTC timestamp (yyyy-MM-dd HH:mm:ss), RFC3339 or epoch seconds",
		)
	}

	return n, nil
}

func matchFilter(actual []string, created float64, name, op string, values []string) bool {
	negative := op == operatorNotEquals || op == operatorNotContains
	for _, v := range values {
		if name == "CreationTime" {
			n, _ := filterTime(v)
			switch op {
			case operatorEquals:
				if created == n {
					return true
				}
			case operatorNotEquals:
				if created == n {
					return false
				}
			case "LessThan":
				if created < n {
					return true
				}
			case "LessThanOrEqual":
				if created <= n {
					return true
				}
			case "GreaterThan":
				if created > n {
					return true
				}
			case "GreaterThanOrEqual":
				if created >= n {
					return true
				}
			}

			continue
		}

		for _, a := range actual {
			matched := false
			switch op {
			case operatorEquals, "In", operatorNotEquals:
				matched = a == v
			case "Contains", operatorNotContains:
				matched = strings.Contains(a, v)
			case "BeginsWith":
				matched = strings.HasPrefix(a, v)
			}

			if matched {
				return !negative
			}
		}
	}

	return negative
}

func (s *Service) ListLocations(ctx context.Context, in *api.ListLocationsRequest) (*api.ListLocationsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	for _, f := range in.Filters {
		if filterErr := checkFilter(common.Value(f.Name), common.Value(f.Operator), f.Values, false); filterErr != nil {
			return nil, filterErr
		}
	}

	items := []api.LocationListEntry{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, e := storage.Records[locationRecord](tx, scope.Prefix(serviceName, kindLocation))
		if e != nil {
			return e
		}

		for _, r := range records {
			matched := true
			for _, f := range in.Filters {
				actual := r.URI
				if common.Value(f.Name) == "LocationType" {
					actual = r.Kind
				}

				if !matchFilter(
					[]string{actual},
					r.Created,
					common.Value(f.Name),
					common.Value(f.Operator),
					f.Values,
				) {
					matched = false
					break
				}
			}

			if matched {
				items = append(items, api.LocationListEntry{LocationArn: new(r.ARN), LocationUri: new(r.URI)})
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		items,
		func(r api.LocationListEntry) string { return common.Value(r.LocationArn) },
		in.NextToken,
		pageLimit(in.MaxResults),
		invalid("Invalid NextToken"),
	)
	return &api.ListLocationsResponse{Locations: page, NextToken: next}, err
}

func (s *Service) ListTasks(ctx context.Context, in *api.ListTasksRequest) (*api.ListTasksResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	for _, f := range in.Filters {
		if filterErr := checkFilter(common.Value(f.Name), common.Value(f.Operator), f.Values, true); filterErr != nil {
			return nil, filterErr
		}
	}

	items := []api.TaskListEntry{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, e := storage.Records[taskRecord](tx, scope.Prefix(serviceName, kindTask))
		if e != nil {
			return e
		}

		for _, r := range records {
			matched := true
			for _, f := range in.Filters {
				refs := []string{common.Value(r.SourceLocationArn), common.Value(r.DestinationLocationArn)}
				for _, ref := range []string{common.Value(r.SourceLocationArn), common.Value(r.DestinationLocationArn)} {
					refs = append(refs, ref[strings.LastIndex(ref, "/")+1:])
				}

				if !matchFilter(
					refs,
					common.Value(r.CreationTime),
					common.Value(f.Name),
					common.Value(f.Operator),
					f.Values,
				) {
					matched = false
					break
				}
			}

			if matched {
				items = append(
					items,
					api.TaskListEntry{TaskArn: r.TaskArn, Name: r.Name, Status: r.Status, TaskMode: r.TaskMode},
				)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		items,
		func(r api.TaskListEntry) string { return common.Value(r.TaskArn) },
		in.NextToken,
		pageLimit(in.MaxResults),
		invalid("Invalid NextToken"),
	)
	return &api.ListTasksResponse{Tasks: page, NextToken: next}, err
}

func (s *Service) ListTaskExecutions(
	ctx context.Context,
	in *api.ListTaskExecutionsRequest,
) (*api.ListTaskExecutionsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	items := []api.TaskExecutionListEntry{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if in.TaskArn != nil {
			if _, e := readRecord[taskRecord](tx, scope, kindTask, *in.TaskArn); e != nil {
				return e
			}
		}

		records, e := storage.Records[executionRecord](tx, scope.Prefix(serviceName, kindExecution))
		if e != nil {
			return e
		}

		for _, r := range records {
			if in.TaskArn == nil || r.Task == *in.TaskArn {
				items = append(
					items,
					api.TaskExecutionListEntry{
						TaskExecutionArn: r.TaskExecutionArn,
						Status:           r.Status,
						TaskMode:         r.TaskMode,
					},
				)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		items,
		func(r api.TaskExecutionListEntry) string { return common.Value(r.TaskExecutionArn) },
		in.NextToken,
		pageLimit(in.MaxResults),
		invalid("Invalid NextToken"),
	)
	return &api.ListTaskExecutionsResponse{TaskExecutions: page, NextToken: next}, err
}

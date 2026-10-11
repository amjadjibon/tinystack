package codebuild

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/codebuild"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	sortName  = "NAME"
	ascending = "ASCENDING"
)

type cursor struct {
	Name string
	Time float64
}

func sortValue(p *api.Project, by string) cursor {
	v := cursor{Name: *p.Name}
	if by == "CREATED_TIME" {
		v.Time = common.Value(p.Created)
	}
	if by == "LAST_MODIFIED_TIME" {
		v.Time = common.Value(p.LastModified)
	}
	return v
}

func compare(a, b cursor) int {
	if c := cmp.Compare(a.Time, b.Time); c != 0 {
		return c
	}
	return strings.Compare(a.Name, b.Name)
}

func (s *Service) ListProjects(ctx context.Context, in *api.ListProjectsInput) (*api.ListProjectsOutput, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	by := common.Value(common.OrElse(in.SortBy, new(sortName)))
	order := common.Value(common.OrElse(in.SortOrder, new(ascending)))
	if by != sortName && by != "CREATED_TIME" && by != "LAST_MODIFIED_TIME" {
		return nil, invalid("Invalid SortBy")
	}
	direction := 1
	if order == "DESCENDING" {
		direction = -1
	} else if order != ascending {
		return nil, invalid("Invalid SortOrder")
	}
	sum := sha256.Sum256([]byte(scope.AccountID + "/" + scope.Region + "/ListProjects/" + by + "/" + order))
	prefix := hex.EncodeToString(sum[:]) + ":"
	var after *cursor
	if in.NextToken != nil {
		raw, err := base64.RawURLEncoding.DecodeString(*in.NextToken)
		v := cursor{}
		if err != nil || !strings.HasPrefix(string(raw), prefix) ||
			json.Unmarshal([]byte(strings.TrimPrefix(string(raw), prefix)), &v) != nil ||
			!namePattern.MatchString(v.Name) ||
			v.Time < 0 {
			return nil, invalid("Invalid NextToken for this scope and sort")
		}
		after = &v
	}
	var rows []*api.Project
	e = s.store.View(ctx, func(tx storage.ReadTx) error {
		var err error
		rows, err = storage.Records[api.Project](tx, scope.Prefix(serviceName, projectKind))
		return err
	})
	if e != nil {
		return nil, e
	}
	slices.SortFunc(
		rows,
		func(a, b *api.Project) int { return direction * compare(sortValue(a, by), sortValue(b, by)) },
	)
	if after != nil {
		rows = slices.DeleteFunc(
			rows,
			func(p *api.Project) bool { return direction*compare(sortValue(p, by), *after) <= 0 },
		)
	}
	out := &api.ListProjectsOutput{Projects: []string{}}
	selected := rows
	if len(selected) > 100 {
		selected = selected[:100]
	}
	for _, p := range selected {
		out.Projects = append(out.Projects, *p.Name)
	}
	if len(rows) > 100 {
		raw, err := json.Marshal(sortValue(selected[99], by))
		if err != nil {
			return nil, err
		}
		out.NextToken = new(base64.RawURLEncoding.EncodeToString([]byte(prefix + string(raw))))
	}
	return out, nil
}

package cloudwatch

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudwatch"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	dashboardPage   = 1000
	notFoundBoard   = "DashboardNotFoundError"
	invalidBoard    = "DashboardInvalidInputError"
	maxDashboardLen = 1 << 20
)

var dashboardName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)

// dashboard is a dashboard's record.
type dashboard struct {
	Tags     map[string]string
	Name     string
	Body     string
	Modified float64
}

func dashboardKey(scope request.Scope, name string) string {
	return scope.Prefix(service, kindDashboard) + name
}

func (s *Service) PutDashboard(ctx context.Context, in *api.PutDashboardInput) (*api.PutDashboardOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name, body := common.Value(in.DashboardName), common.Value(in.DashboardBody)
	switch {
	case name == "":
		return nil, missing("DashboardName")
	case !dashboardName.MatchString(name):
		return nil, awserror.New(invalidBoard,
			"The dashboard name must be 1 to 255 letters, digits, hyphens, and underscores")
	case body == "":
		return nil, missing("DashboardBody")
	case len(body) > maxDashboardLen:
		return nil, awserror.New(invalidBoard, "The dashboard body must be at most 1 MB")
	}

	var doc struct {
		Widgets []json.RawMessage `json:"widgets"`
	}
	if json.Unmarshal([]byte(body), &doc) != nil || doc.Widgets == nil {
		return nil, awserror.New(invalidBoard, "The dashboard body must be a JSON object with a widgets array")
	}

	tags := tagsOf(in.Tags)
	if err = tagging.Check(tags); err != nil {
		return nil, invalidValue("%s", err.Error())
	}

	return &api.PutDashboardOutput{DashboardValidationMessages: []api.DashboardValidationMessage{}},
		s.store.Update(ctx, func(tx storage.WriteTx) error {
			old, found, readErr := storage.Read[dashboard](tx, dashboardKey(m.Scope, name))
			if readErr != nil {
				return readErr
			}

			if found {
				tags = old.Tags
			}

			return storage.Write(tx, dashboardKey(m.Scope, name),
				dashboard{Name: name, Body: body, Modified: common.Unix(s.now()), Tags: tags})
		})
}

func (s *Service) GetDashboard(ctx context.Context, in *api.GetDashboardInput) (*api.GetDashboardOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var d *dashboard
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var found bool
		d, found, err = storage.Read[dashboard](tx, dashboardKey(m.Scope, common.Value(in.DashboardName)))
		if err == nil && !found {
			err = awserror.New(notFoundBoard, "Dashboard "+common.Value(in.DashboardName)+" does not exist")
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	return &api.GetDashboardOutput{
		DashboardArn: new(dashboardARN(m.Scope, d.Name)), DashboardBody: new(d.Body), DashboardName: new(d.Name),
	}, nil
}

func (s *Service) ListDashboards(ctx context.Context, in *api.ListDashboardsInput) (*api.ListDashboardsOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListDashboardsOutput{DashboardEntries: []api.DashboardEntry{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		p := m.Prefix(service, kindDashboard)
		for _, k := range tx.Keys(p + common.Value(in.DashboardNamePrefix)) {
			name := strings.TrimPrefix(k, p)
			if in.NextToken != nil && name <= *in.NextToken {
				continue
			}

			if len(out.DashboardEntries) == dashboardPage {
				out.NextToken = out.DashboardEntries[dashboardPage-1].DashboardName
				return nil
			}

			d, _, readErr := storage.Read[dashboard](tx, k)
			if readErr != nil {
				return readErr
			}

			out.DashboardEntries = append(out.DashboardEntries, api.DashboardEntry{
				DashboardArn: new(dashboardARN(m.Scope, name)), DashboardName: new(name),
				LastModified: new(d.Modified), Size: new(int64(len(d.Body))),
			})
		}

		return nil
	})

	return out, err
}

func (s *Service) DeleteDashboards(
	ctx context.Context, in *api.DeleteDashboardsInput,
) (*api.DeleteDashboardsOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if len(in.DashboardNames) == 0 {
		return nil, missing("DashboardNames")
	}

	return &api.DeleteDashboardsOutput{}, s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, name := range in.DashboardNames {
			if _, ok := tx.Get(dashboardKey(m.Scope, name)); !ok {
				return awserror.New(notFoundBoard, "Dashboard "+name+" does not exist")
			}
		}

		for _, name := range in.DashboardNames {
			tx.Delete(dashboardKey(m.Scope, name))
		}

		return nil
	})
}

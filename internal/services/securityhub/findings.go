package securityhub

import (
	"context"
	"encoding/base64"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/securityhub"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func findingID(product, id *string) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(common.Value(product)),
	) + "/" + base64.RawURLEncoding.EncodeToString(
		[]byte(common.Value(id)),
	)
}

func checkFinding(f *api.AwsSecurityFinding) error {
	if err := f.Validate(""); err != nil {
		return err
	}

	if f.Severity == nil {
		return invalid("Severity is required.")
	}

	for _, field := range []string{
		"SchemaVersion", "Id", "ProductArn", "GeneratorId", "AwsAccountId",
		fieldCreatedAt, fieldUpdatedAt, "Title", "Description",
	} {
		if len(findingValues(f, field)) == 0 || findingValues(f, field)[0] == "" {
			return invalid(field + " is required.")
		}
	}

	if len(f.Resources) == 0 || len(f.Types) == 0 {
		return invalid("Resources and Types are required.")
	}

	for _, date := range []*string{f.CreatedAt, f.UpdatedAt} {
		if _, err := time.Parse(time.RFC3339Nano, common.Value(date)); err != nil {
			return invalid("CreatedAt and UpdatedAt must be RFC3339 timestamps.")
		}
	}

	return nil
}

func (s *Service) BatchImportFindings(
	ctx context.Context,
	in *api.BatchImportFindingsRequest,
) (*api.BatchImportFindingsResponse, error) {
	out := &api.BatchImportFindingsResponse{
		FailedCount:    new(int64(0)),
		SuccessCount:   new(int64(0)),
		FailedFindings: []api.ImportFindingsError{},
	}

	err := s.transaction(ctx, func(tx storage.WriteTx, scope request.Scope, _ *hubRecord) error {
		if len(in.Findings) < 1 || len(in.Findings) > 100 {
			return invalid("Findings must contain between 1 and 100 entries.")
		}

		for i := range in.Findings {
			f := in.Findings[i]
			if err := checkFinding(&f); err != nil {
				*out.FailedCount++
				out.FailedFindings = append(
					out.FailedFindings,
					api.ImportFindingsError{Id: f.Id, ErrorCode: new("InvalidInput"), ErrorMessage: new(err.Error())},
				)
				continue
			}

			if f.RecordState == nil {
				f.RecordState = new("ACTIVE")
			}

			if f.Workflow == nil {
				f.Workflow = &api.Workflow{Status: new("NEW")}
			}

			if err := storage.Write(
				tx,
				scope.Key(serviceName, "finding", findingID(f.ProductArn, f.Id)),
				f,
			); err != nil {
				return err
			}

			*out.SuccessCount++
		}

		return nil
	})

	return out, err
}

func (s *Service) GetFindings(ctx context.Context, in *api.GetFindingsRequest) (*api.GetFindingsResponse, error) {
	out := &api.GetFindingsResponse{}
	err := s.transaction(ctx, func(tx storage.WriteTx, scope request.Scope, _ *hubRecord) error {
		if err := checkFilters(in.Filters); err != nil {
			return err
		}

		for _, criterion := range in.SortCriteria {
			field := common.Value(criterion.Field)
			if !slices.Contains(stringFields, field) && field != fieldCreatedAt && field != fieldUpdatedAt &&
				field != "Title" {
				return invalid("Unsupported sort field: " + field)
			}

			if field == fieldResourceType || field == fieldResourceID {
				return invalid("Sorting by multivalued resource fields is not supported.")
			}

			if common.Value(criterion.SortOrder) != "asc" && common.Value(criterion.SortOrder) != "desc" {
				return invalid("SortOrder must be asc or desc.")
			}
		}

		records, err := storage.Records[api.AwsSecurityFinding](tx, scope.Prefix(serviceName, "finding"))
		if err != nil {
			return err
		}

		items := []api.AwsSecurityFinding{}
		for _, f := range records {
			if matches(f, in.Filters) {
				items = append(items, *f)
			}
		}

		sort.SliceStable(items, func(i, j int) bool {
			for _, criterion := range in.SortCriteria {
				field := common.Value(criterion.Field)
				a, b := strings.Join(
					findingValues(&items[i], field),
					"",
				), strings.Join(
					findingValues(&items[j], field),
					"",
				)
				cmp := strings.Compare(a, b)
				if field == fieldCreatedAt || field == fieldUpdatedAt {
					ta, _ := time.Parse(time.RFC3339Nano, a)
					tb, _ := time.Parse(time.RFC3339Nano, b)
					cmp = ta.Compare(tb)
				}

				if cmp != 0 {
					if common.Value(criterion.SortOrder) == "desc" {
						return cmp > 0
					}

					return cmp < 0
				}
			}

			return false
		})

		// Page IDs are positions in this sorted snapshot, rather than finding IDs:
		// common.Page requires monotonic IDs even when the requested order is descending.
		type indexed struct {
			Finding *api.AwsSecurityFinding
			Index   int
		}

		ordered := make([]indexed, 0, len(items))
		for i := range items {
			ordered = append(ordered, indexed{Finding: &items[i], Index: i})
		}

		selected, next, err := page(
			ordered,
			func(v indexed) string { return fmt.Sprintf("%020d", v.Index) },
			in.NextToken,
			in.MaxResults,
		)
		if err != nil {
			return err
		}

		out.Findings = []api.AwsSecurityFinding{}
		for _, v := range selected {
			out.Findings = append(out.Findings, *v.Finding)
		}

		out.NextToken = next
		return nil
	})

	return out, err
}

func (s *Service) BatchUpdateFindings(
	ctx context.Context,
	in *api.BatchUpdateFindingsRequest,
) (*api.BatchUpdateFindingsResponse, error) {
	out := &api.BatchUpdateFindingsResponse{
		ProcessedFindings:   []api.AwsSecurityFindingIdentifier{},
		UnprocessedFindings: []api.BatchUpdateFindingsUnprocessedFinding{},
	}

	err := s.transaction(ctx, func(tx storage.WriteTx, scope request.Scope, _ *hubRecord) error {
		for _, id := range in.FindingIdentifiers {
			key := scope.Key(serviceName, "finding", findingID(id.ProductArn, id.Id))
			f, found, err := storage.Read[api.AwsSecurityFinding](tx, key)
			if err != nil {
				return err
			}

			if !found {
				out.UnprocessedFindings = append(
					out.UnprocessedFindings,
					api.BatchUpdateFindingsUnprocessedFinding{
						FindingIdentifier: new(id),
						ErrorCode:         new("FindingNotFound"),
						ErrorMessage:      new("Finding was not found."),
					},
				)
				continue
			}

			updated := now()
			if in.Workflow != nil {
				f.Workflow = &api.Workflow{Status: in.Workflow.Status}
			}

			if in.Severity != nil {
				if f.Severity == nil {
					f.Severity = &api.Severity{}
				}

				if in.Severity.Label != nil {
					f.Severity.Label = in.Severity.Label
				}

				if in.Severity.Normalized != nil {
					f.Severity.Normalized = in.Severity.Normalized
				}

				if in.Severity.Product != nil {
					f.Severity.Product = in.Severity.Product
				}
			}

			if in.Note != nil {
				f.Note = &api.Note{Text: in.Note.Text, UpdatedBy: in.Note.UpdatedBy, UpdatedAt: new(updated)}
			}

			input, output := reflect.ValueOf(in).Elem(), reflect.ValueOf(f).Elem()
			for _, field := range []string{
				"Confidence", "Criticality", "VerificationState", "Types", "UserDefinedFields", "RelatedFindings",
			} {
				value := input.FieldByName(field)
				if !value.IsNil() {
					output.FieldByName(field).Set(value)
				}
			}

			f.UpdatedAt = new(updated)
			if err := storage.Write(tx, key, f); err != nil {
				return err
			}

			out.ProcessedFindings = append(out.ProcessedFindings, id)
		}

		return nil
	})

	return out, err
}

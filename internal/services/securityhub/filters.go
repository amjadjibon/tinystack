package securityhub

import (
	"reflect"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/securityhub"
	"github.com/amjadjibon/tinystack/internal/common"
)

var stringFields = []string{
	"Id",
	"ProductArn",
	"GeneratorId",
	"AwsAccountId",
	"SeverityLabel",
	"WorkflowStatus",
	"RecordState",
	fieldResourceType,
	fieldResourceID,
	"ComplianceStatus",
}

func findingValues(f *api.AwsSecurityFinding, field string) []string {
	switch field {
	case "SeverityLabel":
		if f.Severity != nil {
			return []string{common.Value(f.Severity.Label)}
		}
	case "WorkflowStatus":
		if f.Workflow != nil {
			return []string{common.Value(f.Workflow.Status)}
		}
	case "ComplianceStatus":
		if f.Compliance != nil {
			return []string{common.Value(f.Compliance.Status)}
		}
	case fieldResourceType, fieldResourceID:
		out := []string{}
		for _, r := range f.Resources {
			if field == fieldResourceType {
				out = append(out, common.Value(r.Type))
			} else {
				out = append(out, common.Value(r.Id))
			}
		}

		return out
	default:
		v := reflect.ValueOf(f).Elem().FieldByName(field)
		if v.IsValid() && v.Kind() == reflect.Pointer && !v.IsNil() {
			return []string{v.Elem().String()}
		}
	}

	return nil
}

func checkFilters(filters *api.AwsSecurityFindingFilters) error {
	if filters == nil {
		return nil
	}

	v := reflect.ValueOf(filters).Elem()
	for _, field := range stringFields {
		terms := v.FieldByName(field).Interface().([]api.StringFilter)
		positive, negative := false, false
		for _, term := range terms {
			switch common.Value(term.Comparison) {
			case "EQUALS", comparisonPrefix:
				positive = true
			case "NOT_EQUALS":
				negative = true
			default:
				return invalid("Only EQUALS, PREFIX and NOT_EQUALS string comparisons are supported.")
			}
		}

		if positive && negative {
			return invalid("Positive and negative comparisons cannot be mixed for a field.")
		}
	}

	for _, terms := range [][]api.DateFilter{filters.CreatedAt, filters.UpdatedAt} {
		for _, term := range terms {
			if term.Start == nil && term.End == nil {
				return invalid("Date filters require Start or End.")
			}

			var start, end time.Time
			var err error
			if term.Start != nil {
				start, err = time.Parse(time.RFC3339Nano, *term.Start)
				if err != nil {
					return invalid("Invalid date filter Start.")
				}
			}

			if term.End != nil {
				end, err = time.Parse(time.RFC3339Nano, *term.End)
				if err != nil {
					return invalid("Invalid date filter End.")
				}
			}

			if !start.IsZero() && !end.IsZero() && start.After(end) {
				return invalid("Date filter Start must not follow End.")
			}
		}
	}

	return nil
}

func matches(f *api.AwsSecurityFinding, filters *api.AwsSecurityFindingFilters) bool {
	if filters == nil {
		return true
	}

	v := reflect.ValueOf(filters).Elem()
	for _, field := range stringFields {
		terms := v.FieldByName(field).Interface().([]api.StringFilter)
		if len(terms) == 0 {
			continue
		}

		values := findingValues(f, field)
		negative := common.Value(terms[0].Comparison) == "NOT_EQUALS"
		matched := false
		for _, term := range terms {
			for _, value := range values {
				if (common.Value(term.Comparison) == comparisonPrefix && strings.HasPrefix(value, common.Value(term.Value))) ||
					(common.Value(term.Comparison) != comparisonPrefix && value == common.Value(term.Value)) {
					matched = true
				}
			}
		}

		if matched == negative {
			return false
		}
	}

	for field, terms := range map[string][]api.DateFilter{
		fieldCreatedAt: filters.CreatedAt, fieldUpdatedAt: filters.UpdatedAt,
	} {
		if len(terms) == 0 {
			continue
		}

		values := findingValues(f, field)
		if len(values) == 0 {
			return false
		}

		date, err := time.Parse(time.RFC3339Nano, values[0])
		if err != nil {
			return false
		}

		matched := false
		for _, term := range terms {
			start, _ := time.Parse(time.RFC3339Nano, common.Value(term.Start))
			end, _ := time.Parse(time.RFC3339Nano, common.Value(term.End))
			if (term.Start == nil || !date.Before(start)) && (term.End == nil || !date.After(end)) {
				matched = true
			}
		}

		if !matched {
			return false
		}
	}

	return true
}

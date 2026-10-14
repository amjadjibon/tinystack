// Package costandusagereportservice stores account-wide CUR definitions and tags.
// It does not calculate costs, validate delivery permissions, or write report files.
package costandusagereportservice

import (
	"context"
	"regexp"
	"slices"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/costandusagereportservice"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const serviceName = "costandusagereportservice"

var _ api.API = (*Service)(nil)

type (
	Service      struct{ store storage.Store }
	reportRecord struct {
		Definition api.ReportDefinition
		Tags       map[string]string
	}
)

func New(store storage.Store) *Service { return &Service{store: store} }

var (
	namePattern   = regexp.MustCompile(`^[0-9A-Za-z!\-_.*'()]+$`)
	bucketPattern = regexp.MustCompile(`^[A-Za-z0-9_.\-]+$`)
	prefixPattern = regexp.MustCompile(`^[0-9A-Za-z!\-_.*'()/]*$`)
)

func checkString(member string, value *string, pattern *regexp.Regexp) error {
	if value == nil || utf8.RuneCountInString(*value) > 256 || !pattern.MatchString(*value) {
		return awserror.Validation("%s does not satisfy the model's length or pattern constraints", member)
	}

	return nil
}

func checkDefinition(d *api.ReportDefinition) error {
	if d == nil {
		return awserror.Validation("ReportDefinition is required")
	}

	for _, field := range []struct {
		value   *string
		pattern *regexp.Regexp
		name    string
	}{
		{d.ReportName, namePattern, "ReportName"},
		{d.S3Bucket, bucketPattern, "S3Bucket"},
		{d.S3Prefix, prefixPattern, "S3Prefix"},
	} {
		if checkErr := checkString(field.name, field.value, field.pattern); checkErr != nil {
			return checkErr
		}
	}

	for _, field := range []struct {
		name    string
		value   *string
		allowed []string
	}{
		{"TimeUnit", d.TimeUnit, []string{"HOURLY", "DAILY", "MONTHLY"}},
		{"Format", d.Format, []string{"textORcsv", "Parquet"}},
		{"Compression", d.Compression, []string{"ZIP", "GZIP", "Parquet"}},
		{"S3Region", d.S3Region, s3Regions},
	} {
		if !slices.Contains(field.allowed, common.Value(field.value)) {
			return awserror.Validation("%s is not a modeled enum value", field.name)
		}
	}

	if d.AdditionalSchemaElements == nil {
		return awserror.Validation("AdditionalSchemaElements is required")
	}

	for _, v := range d.AdditionalSchemaElements {
		if !slices.Contains([]string{"RESOURCES", "SPLIT_COST_ALLOCATION_DATA", "MANUAL_DISCOUNT_COMPATIBILITY"}, v) {
			return awserror.Validation("AdditionalSchemaElements contains an invalid value")
		}
	}

	for _, v := range d.AdditionalArtifacts {
		if !slices.Contains([]string{"REDSHIFT", "QUICKSIGHT", "ATHENA"}, v) {
			return awserror.Validation("AdditionalArtifacts contains an invalid value")
		}
	}

	if d.ReportVersioning != nil &&
		!slices.Contains([]string{"CREATE_NEW_REPORT", "OVERWRITE_REPORT"}, *d.ReportVersioning) {
		return awserror.Validation("ReportVersioning is not a modeled enum value")
	}

	return nil
}

func (s *Service) PutReportDefinition(ctx context.Context, in *api.PutReportDefinitionRequest) (
	*api.PutReportDefinitionResponse, error,
) {
	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return nil, err
	}

	if checkErr := checkDefinition(in.ReportDefinition); checkErr != nil {
		return nil, checkErr
	}

	tags, err := checkTags(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := scope.Key(serviceName, "report", *in.ReportDefinition.ReportName)
		if _, exists := tx.Get(key); exists {
			return awserror.New("DuplicateReportNameException", "A report with this name already exists")
		}

		return storage.Write(tx, key, reportRecord{Definition: *in.ReportDefinition, Tags: tags})
	})
	return &api.PutReportDefinitionResponse{}, err
}

func (s *Service) DescribeReportDefinitions(ctx context.Context, in *api.DescribeReportDefinitionsRequest) (
	*api.DescribeReportDefinitionsResponse, error,
) {
	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in.NextToken != nil && len(*in.NextToken) > 256 {
		return nil, awserror.Validation("NextToken must contain at most 256 characters")
	}

	// The pinned model specifies both minimum and maximum as five.
	if in.MaxResults != nil && *in.MaxResults != 5 {
		return nil, awserror.Validation("MaxResults must be 5")
	}

	out := &api.DescribeReportDefinitionsResponse{ReportDefinitions: []api.ReportDefinition{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, e := storage.Records[reportRecord](tx, scope.Prefix(serviceName, "report"))
		if e != nil {
			return e
		}

		selected, next, e := common.Page(records, func(r *reportRecord) string { return *r.Definition.ReportName },
			in.NextToken, 5, awserror.Validation("NextToken is invalid"))
		if e != nil {
			return e
		}

		for _, r := range selected {
			out.ReportDefinitions = append(out.ReportDefinitions, r.Definition)
		}

		out.NextToken = next
		return nil
	})
	return out, err
}

func (s *Service) ModifyReportDefinition(ctx context.Context, in *api.ModifyReportDefinitionRequest) (
	*api.ModifyReportDefinitionResponse, error,
) {
	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return nil, err
	}

	if checkErr := checkString("ReportName", in.ReportName, namePattern); checkErr != nil {
		return nil, checkErr
	}

	if checkErr := checkDefinition(in.ReportDefinition); checkErr != nil {
		return nil, checkErr
	}

	if *in.ReportName != *in.ReportDefinition.ReportName {
		return nil, awserror.Validation("ReportName cannot be changed")
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := scope.Key(serviceName, "report", *in.ReportName)
		r, exists, e := storage.Read[reportRecord](tx, key)
		if e != nil {
			return e
		}

		if !exists {
			return awserror.Validation("Report does not exist")
		}

		r.Definition = *in.ReportDefinition
		return storage.Write(tx, key, r)
	})
	return &api.ModifyReportDefinitionResponse{}, err
}

func (s *Service) DeleteReportDefinition(ctx context.Context, in *api.DeleteReportDefinitionRequest) (
	*api.DeleteReportDefinitionResponse, error,
) {
	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return nil, err
	}

	if checkErr := checkString("ReportName", in.ReportName, namePattern); checkErr != nil {
		return nil, checkErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Delete(scope.Key(serviceName, "report", *in.ReportName))
		return nil
	})
	return &api.DeleteReportDefinitionResponse{ResponseMessage: new("SUCCESS")}, err
}

// s3Regions is the AWSRegion enum from the pinned model.
var s3Regions = []string{
	"af-south-1",
	"ap-east-1",
	"ap-south-1",
	"ap-south-2",
	"ap-southeast-1",
	"ap-southeast-2",
	"ap-southeast-3",
	"ap-northeast-1",
	"ap-northeast-2",
	"ap-northeast-3",
	"ca-central-1",
	"eu-central-1",
	"eu-central-2",
	"eu-west-1",
	"eu-west-2",
	"eu-west-3",
	"eu-north-1",
	"eu-south-1",
	"eu-south-2",
	"me-central-1",
	"me-south-1",
	"sa-east-1",
	"us-east-1",
	"us-east-2",
	"us-west-1",
	"us-west-2",
	"cn-north-1",
	"cn-northwest-1",
}

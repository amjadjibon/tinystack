package redshiftdata

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/csv"
	"strconv"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/redshiftdata"
	"github.com/amjadjibon/tinystack/internal/common"
)

func (s *Service) result(ctx context.Context, id *string, format string) (*Fixture, error) {
	parent, child := splitID(common.Value(id))
	r, err := s.get(ctx, new(parent))
	if err != nil {
		return nil, err
	}

	if common.Value(r.Status) != statusFinished {
		return nil, invalid("Statement has not finished successfully")
	}

	if common.Value(r.ResultFormat) != format {
		return nil, invalid("Result format does not match this result operation")
	}

	index := 0
	if r.Batch {
		if child == "" {
			return nil, invalid("Use a sub-statement Id to fetch batch results")
		}

		index = -1
		for i, sub := range r.SubStatements {
			if common.Value(sub.Id) == common.Value(id) {
				index = i
				break
			}
		}

		if index < 0 {
			return nil, missing()
		}
	} else if child != "" {
		return nil, missing()
	}

	f := r.Answers[index]
	if len(f.ColumnMetadata) == 0 {
		return nil, invalid("Statement does not have a result set")
	}

	return &f, nil
}

func pageRecords(records [][]api.Field, token *string) ([][]api.Field, *string, error) {
	// An ordinal cursor is stable because statement results are immutable.
	start := 0
	if token != nil {
		raw, err := base64.RawURLEncoding.DecodeString(*token)
		if err != nil {
			return nil, nil, invalid(invalidTokenMessage)
		}

		start, err = strconv.Atoi(string(raw))
		if err != nil || start < 0 || start >= len(records) {
			return nil, nil, invalid(invalidTokenMessage)
		}
	}

	end := min(start+1000, len(records))
	var next *string
	if end < len(records) {
		next = new(base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(end))))
	}

	return records[start:end], next, nil
}

func (s *Service) GetStatementResult(
	ctx context.Context,
	in *api.GetStatementResultRequest,
) (*api.GetStatementResultResponse, error) {
	f, err := s.result(ctx, in.Id, formatJSON)
	if err != nil {
		return nil, err
	}

	rows, token, err := pageRecords(f.Records, in.NextToken)
	if err != nil {
		return nil, err
	}

	return &api.GetStatementResultResponse{
		Records:        rows,
		ColumnMetadata: f.ColumnMetadata,
		NextToken:      token,
		TotalNumRows:   new(int64(len(f.Records))),
	}, nil
}

func csvValue(f api.Field) string {
	switch {
	case f.StringValue != nil:
		return *f.StringValue
	case f.LongValue != nil:
		return strconv.FormatInt(*f.LongValue, 10)
	case f.DoubleValue != nil:
		return strconv.FormatFloat(*f.DoubleValue, 'g', -1, 64)
	case f.BooleanValue != nil:
		return strconv.FormatBool(*f.BooleanValue)
	case f.BlobValue != nil:
		return base64.StdEncoding.EncodeToString(*f.BlobValue)
	default:
		return ""
	}
}

func (s *Service) GetStatementResultV2(
	ctx context.Context,
	in *api.GetStatementResultV2Request,
) (*api.GetStatementResultV2Response, error) {
	f, err := s.result(ctx, in.Id, formatCSV)
	if err != nil {
		return nil, err
	}

	rows, token, err := pageRecords(f.Records, in.NextToken)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	writer := csv.NewWriter(&buf)
	if in.NextToken == nil {
		header := make([]string, len(f.ColumnMetadata))
		for i, col := range f.ColumnMetadata {
			header[i] = common.Value(common.OrElse(col.Label, col.Name))
		}

		if err := writer.Write(header); err != nil {
			return nil, err
		}
	}

	for _, row := range rows {
		values := make([]string, len(row))
		for i, v := range row {
			values[i] = csvValue(v)
		}

		if err := writer.Write(values); err != nil {
			return nil, err
		}
	}

	writer.Flush()
	if err := writer.Error(); err != nil {
		return nil, err
	}

	return &api.GetStatementResultV2Response{
		Records:        []api.QueryRecords{{CSVRecords: new(buf.String())}},
		ColumnMetadata: f.ColumnMetadata,
		ResultFormat:   new(formatCSV),
		TotalNumRows:   new(int64(len(f.Records))),
		NextToken:      token,
	}, nil
}

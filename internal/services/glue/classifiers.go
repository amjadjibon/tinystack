package glue

import (
	"context"
	"strings"
	"unicode/utf8"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/glue"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tabular"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const kindClassifier = "classifier"

func classifierName(c *api.Classifier) string {
	if c.CsvClassifier != nil {
		return common.Value(c.CsvClassifier.Name)
	}

	return common.Value(c.JsonClassifier.Name)
}

func loadClassifier(tx storage.ReadTx, scope request.Scope, n string) (*api.Classifier, error) {
	if err := orchestrationName(new(n)); err != nil {
		return nil, err
	}

	r, ok, err := storage.Read[api.Classifier](tx, orchestrationKey(scope, kindClassifier, n))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, notFound("Classifier %s not found", n)
	}

	return r, nil
}

func validateClassifier(c *api.Classifier) error {
	if (c.CsvClassifier == nil) == (c.JsonClassifier == nil) {
		return invalid("Exactly one classifier is required")
	}

	if err := orchestrationName(new(classifierName(c))); err != nil {
		return err
	}

	if j := c.JsonClassifier; j != nil {
		if !tabular.ValidJSONPath(common.Value(j.JsonPath)) {
			return invalid("Supported JsonPath: $, $[*], or $.member paths ending optionally in [*]")
		}

		return nil
	}

	v := c.CsvClassifier
	for _, p := range []*string{v.Delimiter, v.QuoteSymbol} {
		if p != nil && (utf8.RuneCountInString(*p) != 1 || strings.ContainsAny(*p, "\r\n\x00")) {
			return invalid("Delimiter and QuoteSymbol must be single characters")
		}
	}

	if v.Delimiter != nil && v.QuoteSymbol != nil && *v.Delimiter == *v.QuoteSymbol {
		return invalid("Delimiter and QuoteSymbol must differ")
	}

	h := common.Value(v.ContainsHeader)
	if h != "" && h != "PRESENT" && h != "ABSENT" && h != "UNKNOWN" {
		return invalid("ContainsHeader must be PRESENT, ABSENT or UNKNOWN")
	}

	if !common.Value(v.CustomDatatypeConfigured) && len(v.CustomDatatypes) > 0 {
		return invalid("CustomDatatypes requires CustomDatatypeConfigured")
	}

	for _, t := range v.CustomDatatypes {
		if !tabular.ValidCrawlType(t) {
			return invalid("Unsupported custom datatype %s", t)
		}
	}

	for _, h := range v.Header {
		if h == "" {
			return invalid("Header names cannot be empty")
		}
	}

	return nil
}

func (s *Service) CreateClassifier(
	ctx context.Context,
	in *api.CreateClassifierRequest,
) (*api.CreateClassifierResponse, error) {
	c := &api.Classifier{}
	now := common.Unix(s.now())
	if v := in.CsvClassifier; v != nil {
		c.CsvClassifier = &api.CsvClassifier{
			Name:                     v.Name,
			CreationTime:             new(now),
			Version:                  new(int64(1)),
			Delimiter:                v.Delimiter,
			QuoteSymbol:              v.QuoteSymbol,
			ContainsHeader:           v.ContainsHeader,
			Header:                   v.Header,
			DisableValueTrimming:     v.DisableValueTrimming,
			AllowSingleColumn:        v.AllowSingleColumn,
			CustomDatatypeConfigured: v.CustomDatatypeConfigured,
			CustomDatatypes:          v.CustomDatatypes,
		}
	}

	if v := in.JsonClassifier; v != nil {
		c.JsonClassifier = &api.JsonClassifier{
			Name:         v.Name,
			CreationTime: new(now),
			Version:      new(int64(1)),
			JsonPath:     v.JsonPath,
		}
	}

	if err := validateClassifier(c); err != nil {
		return nil, err
	}

	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		key := orchestrationKey(scope, kindClassifier, classifierName(c))
		if _, ok := tx.Get(key); ok {
			return alreadyExists("Classifier already exists")
		}

		return storage.Write(tx, key, c)
	})

	return &api.CreateClassifierResponse{}, err
}

func (s *Service) GetClassifier(ctx context.Context, in *api.GetClassifierRequest) (*api.GetClassifierResponse, error) {
	out := &api.GetClassifierResponse{}
	err := s.view(ctx, nil, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		out.Classifier, err = loadClassifier(tx, scope, common.Value(in.Name))
		return err
	})

	return out, err
}

func (s *Service) GetClassifiers(
	ctx context.Context,
	in *api.GetClassifiersRequest,
) (*api.GetClassifiersResponse, error) {
	out := &api.GetClassifiersResponse{Classifiers: []api.Classifier{}}
	err := s.view(ctx, nil, func(tx storage.ReadTx, scope request.Scope) error {
		rows, err := storage.Records[api.Classifier](tx, scope.Prefix(service, kindClassifier))
		if err != nil {
			return err
		}

		rows, next, err := orchestrationPage(rows, classifierName, in.NextToken, in.MaxResults)
		if err != nil {
			return err
		}

		out.NextToken = next
		for _, r := range rows {
			out.Classifiers = append(out.Classifiers, *r)
		}

		return nil
	})

	return out, err
}

// UpdateClassifier changes only the members the request sets, as Glue does.
func (s *Service) UpdateClassifier(
	ctx context.Context,
	in *api.UpdateClassifierRequest,
) (*api.UpdateClassifierResponse, error) {
	if (in.CsvClassifier == nil) == (in.JsonClassifier == nil) {
		return nil, invalid("Exactly one classifier is required")
	}

	var n string
	if in.CsvClassifier != nil {
		n = common.Value(in.CsvClassifier.Name)
	} else {
		n = common.Value(in.JsonClassifier.Name)
	}

	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		c, err := loadClassifier(tx, scope, n)
		if err != nil {
			return err
		}

		if (c.CsvClassifier == nil) != (in.CsvClassifier == nil) {
			return invalid("Classifier type cannot change")
		}

		now := common.Unix(s.now())
		if v, u := c.CsvClassifier, in.CsvClassifier; v != nil {
			v.Delimiter = common.OrElse(u.Delimiter, v.Delimiter)
			v.QuoteSymbol = common.OrElse(u.QuoteSymbol, v.QuoteSymbol)
			v.ContainsHeader = common.OrElse(u.ContainsHeader, v.ContainsHeader)
			v.DisableValueTrimming = common.OrElse(u.DisableValueTrimming, v.DisableValueTrimming)
			v.AllowSingleColumn = common.OrElse(u.AllowSingleColumn, v.AllowSingleColumn)
			v.CustomDatatypeConfigured = common.OrElse(u.CustomDatatypeConfigured, v.CustomDatatypeConfigured)
			if u.Header != nil {
				v.Header = u.Header
			}

			if u.CustomDatatypes != nil {
				v.CustomDatatypes = u.CustomDatatypes
			}

			v.Version = new(common.Value(v.Version) + 1)
			v.LastUpdated = new(now)
		} else {
			v := c.JsonClassifier
			v.JsonPath = common.OrElse(in.JsonClassifier.JsonPath, v.JsonPath)
			v.Version = new(common.Value(v.Version) + 1)
			v.LastUpdated = new(now)
		}

		if err := validateClassifier(c); err != nil {
			return err
		}

		return storage.Write(tx, orchestrationKey(scope, kindClassifier, n), c)
	})

	return &api.UpdateClassifierResponse{}, err
}

func (s *Service) DeleteClassifier(
	ctx context.Context,
	in *api.DeleteClassifierRequest,
) (*api.DeleteClassifierResponse, error) {
	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := loadClassifier(tx, scope, common.Value(in.Name)); err != nil {
			return err
		}

		tx.Delete(orchestrationKey(scope, kindClassifier, *in.Name))
		return nil
	})

	return &api.DeleteClassifierResponse{}, err
}

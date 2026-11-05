package translate

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/xml"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/translate"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type terminologyRecord struct {
	Tags       map[string]string
	Properties api.TerminologyProperties
	Format     string
	Data       []byte
}

type parallelRecord struct {
	Tags       map[string]string
	Properties api.ParallelDataProperties
}

// parseData validates actual delimited rows or TMX translation units; it never translates their contents.
func parseData(
	data []byte,
	format string,
	terminology bool,
) (languageCodes []string, termCount int64, resultErr error) {
	if !utf8.Valid(data) {
		return nil, 0, invalid("data must be UTF-8")
	}

	if format == "CSV" || format == "TSV" {
		r := csv.NewReader(bytes.NewReader(data))
		if format == "TSV" {
			r.Comma = '\t'
		}

		rows, err := r.ReadAll()
		if err != nil {
			return nil, 0, invalid("invalid %s: %s", format, err)
		}

		if len(rows) < 2 || len(rows[0]) < 2 {
			return nil, 0, invalid("data requires a language header and at least one bilingual row")
		}

		seen := map[string]bool{}
		for _, language := range rows[0] {
			if language == "" || seen[language] {
				return nil, 0, invalid("language headers must be nonempty and unique")
			}

			seen[language] = true
		}

		// Documented quotas: https://docs.aws.amazon.com/translate/latest/dg/what-is-limits.html
		if terminology && len(rows[0]) > 31 {
			return nil, 0, invalid("terminology supports at most 30 target languages")
		}

		for _, row := range rows[1:] {
			size := 0
			for _, term := range row {
				size += len(term)
				if terminology && len(term) > 200 {
					return nil, 0, invalid("terminology terms must not exceed 200 bytes")
				}

				if term == "" {
					return nil, 0, invalid("empty translation cells are unsupported")
				}
			}

			if !terminology && size > 1000 {
				return nil, 0, invalid("parallel data records must not exceed 1000 bytes")
			}
		}

		return rows[0], int64(len(rows) - 1), nil
	}

	if format != "TMX" {
		return nil, 0, invalid("unsupported data format %s", format)
	}

	var doc struct {
		XMLName xml.Name `xml:"tmx"`
		Header  struct {
			Source string `xml:"srclang,attr"`
		} `xml:"header"`
		Body struct {
			Units []struct {
				Variants []struct {
					Lang    string `xml:"http://www.w3.org/XML/1998/namespace lang,attr"`
					Segment string `xml:"seg"`
				} `xml:"tuv"`
			} `xml:"tu"`
		} `xml:"body"`
	}

	decoder := xml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&doc); err != nil {
		return nil, 0, invalid("invalid TMX: %s", err)
	}

	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return nil, 0, invalid("invalid TMX trailing data")
		}

		if text, ok := token.(xml.CharData); !ok || strings.TrimSpace(string(text)) != "" {
			return nil, 0, invalid("invalid TMX trailing content")
		}
	}

	if len(doc.Body.Units) == 0 {
		return nil, 0, invalid("TMX requires translation units")
	}

	var languages []string
	for _, unit := range doc.Body.Units {
		if len(unit.Variants) < 2 {
			return nil, 0, invalid("TMX units must be bilingual")
		}

		if terminology && len(unit.Variants) > 31 {
			return nil, 0, invalid("terminology supports at most 30 target languages")
		}

		var langs []string
		size := 0
		seen := map[string]bool{}
		for _, v := range unit.Variants {
			size += len(v.Segment)
			if terminology && len(v.Segment) > 200 {
				return nil, 0, invalid("terminology terms must not exceed 200 bytes")
			}

			if v.Lang == "" || v.Segment == "" || seen[v.Lang] {
				return nil, 0, invalid("TMX requires unique language variants with segments")
			}

			seen[v.Lang] = true
			langs = append(langs, v.Lang)
		}

		if !terminology && size > 1000 {
			return nil, 0, invalid("parallel data records must not exceed 1000 bytes")
		}

		if languages == nil {
			languages = langs
			if source := doc.Header.Source; source != "" && source != "*all*" {
				if !seen[source] {
					return nil, 0, invalid("TMX source language is missing from its units")
				}

				languages = []string{source}
				for _, language := range langs {
					if language != source {
						languages = append(languages, language)
					}
				}
			}
		}

		if len(languages) != len(langs) {
			return nil, 0, invalid("TMX units must use consistent languages")
		}

		for _, language := range languages {
			if !seen[language] {
				return nil, 0, invalid("TMX units must use consistent languages")
			}
		}
	}

	return languages, int64(len(doc.Body.Units)), nil
}

func (s *Service) ImportTerminology(
	ctx context.Context,
	in *api.ImportTerminologyRequest,
) (*api.ImportTerminologyResponse, error) {
	if common.Value(in.MergeStrategy) != "OVERWRITE" {
		return nil, invalid("only OVERWRITE is supported")
	}

	if err := s.encryption(ctx, in.EncryptionKey); err != nil {
		return nil, err
	}

	if len(in.TerminologyData.File) > 20*1024*1024 {
		return nil, invalid("terminology file exceeds 20 MB")
	}

	langs, count, err := parseData(in.TerminologyData.File, common.Value(in.TerminologyData.Format), true)
	if err != nil {
		return nil, err
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.Name)
	r := terminologyRecord{
		Data:   in.TerminologyData.File,
		Format: common.Value(in.TerminologyData.Format),
		Tags:   tags,
		Properties: api.TerminologyProperties{
			Name:                in.Name,
			Description:         in.Description,
			Arn:                 new(arn(scope, kindTerminology, name)),
			SourceLanguageCode:  new(langs[0]),
			TargetLanguageCodes: langs[1:],
			EncryptionKey:       in.EncryptionKey,
			SizeBytes:           new(int64(len(in.TerminologyData.File))),
			TermCount:           new(count),
			CreatedAt:           new(now()),
			LastUpdatedAt:       new(now()),
			Directionality:      in.TerminologyData.Directionality,
			Format:              in.TerminologyData.Format,
		},
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		old, ok, e := storage.Read[terminologyRecord](tx, scope.Key(serviceName, kindTerminology, name))
		if e != nil {
			return e
		}

		if !ok && len(tx.Keys(scope.Prefix(serviceName, kindTerminology))) >= 100 {
			return invalid("at most 100 terminology files per account and Region")
		}

		if ok {
			r.Properties.CreatedAt = old.Properties.CreatedAt
			r.Properties.Arn = old.Properties.Arn
			if in.Tags == nil {
				r.Tags = old.Tags
			}
		}

		return storage.Write(tx, scope.Key(serviceName, kindTerminology, name), r)
	})
	return &api.ImportTerminologyResponse{TerminologyProperties: new(r.Properties)}, err
}

func (s *Service) GetTerminology(
	ctx context.Context,
	in *api.GetTerminologyRequest,
) (*api.GetTerminologyResponse, error) {
	// AWS requires a retrievable presigned download. No unsigned or fabricated URL is returned.
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		_, e := read[terminologyRecord](tx, scope, kindTerminology, common.Value(in.Name))
		return e
	})
	if err != nil {
		return nil, err
	}

	return nil, invalid(
		"GetTerminology downloads are not emulated; imported bytes are stored but no presigned URL is available",
	)
}

func (s *Service) ListTerminologies(
	ctx context.Context,
	in *api.ListTerminologiesRequest,
) (*api.ListTerminologiesResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	items := []api.TerminologyProperties{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		rows, e := storage.Records[terminologyRecord](tx, scope.Prefix(serviceName, kindTerminology))
		if e != nil {
			return e
		}

		for _, r := range rows {
			items = append(items, r.Properties)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		items,
		func(v api.TerminologyProperties) string { return common.Value(v.Name) },
		in.NextToken,
		limit(in.MaxResults),
		invalid("invalid NextToken"),
	)
	return &api.ListTerminologiesResponse{TerminologyPropertiesList: page, NextToken: next}, err
}

func (s *Service) DeleteTerminology(
	ctx context.Context,
	in *api.DeleteTerminologyRequest,
) (*api.DeleteTerminologyOutput, error) {
	return &api.DeleteTerminologyOutput{}, s.delete(ctx, kindTerminology, common.Value(in.Name))
}

func (s *Service) delete(ctx context.Context, kind, name string) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := scope.Key(serviceName, kind, name)
		if _, ok := tx.Get(key); !ok {
			return missing(name)
		}

		tx.Delete(key)
		tx.Delete(indexPrefix + scope.AccountID + "/" + scope.Region + "/" + kind + "/" + name)
		return nil
	})
}

func (s *Service) parallelInput(
	ctx context.Context,
	cfg *api.ParallelDataConfig,
) (languageCodes []string, recordCount, fileSize int64, resultErr error) {
	bucket, key, err := s3URI(common.Value(cfg.S3Uri))
	if err != nil {
		return nil, 0, 0, err
	}

	data, err := s.buckets.Object(ctx, bucket, key, "")
	if err != nil {
		return nil, 0, 0, invalid("parallel data S3 object must exist: %s", err)
	}

	langs, count, err := parseData(data, common.Value(cfg.Format), false)
	return langs, count, int64(len(data)), err
}

func (s *Service) CreateParallelData(
	ctx context.Context,
	in *api.CreateParallelDataRequest,
) (*api.CreateParallelDataResponse, error) {
	if err := s.encryption(ctx, in.EncryptionKey); err != nil {
		return nil, err
	}

	langs, count, size, err := s.parallelInput(ctx, in.ParallelDataConfig)
	if err != nil {
		return nil, err
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.Name)
	r := parallelRecord{
		Tags: tags,
		Properties: api.ParallelDataProperties{
			Name:                in.Name,
			Arn:                 new(arn(scope, kindParallel, name)),
			Description:         in.Description,
			Status:              new(statusCreating),
			SourceLanguageCode:  new(langs[0]),
			TargetLanguageCodes: langs[1:],
			ParallelDataConfig:  in.ParallelDataConfig,
			ImportedRecordCount: new(count),
			ImportedDataSize:    new(size),
			CreatedAt:           new(now()),
			LastUpdatedAt:       new(now()),
			EncryptionKey:       in.EncryptionKey,
		},
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		tokenKey := idempotency.Key(scope, serviceName, "CreateParallelData", in.ClientToken)
		previous, found, replayErr := idempotency.Replay[parallelRecord](
			tx,
			tokenKey,
			in,
			invalid("parallel data already exists or ClientToken parameters changed"),
		)
		if replayErr != nil {
			return replayErr
		}

		if found {
			r = previous
			return nil
		}

		key := scope.Key(serviceName, kindParallel, name)
		_, exists, e := storage.Read[parallelRecord](tx, key)
		if e != nil {
			return e
		}

		if exists {
			return invalid("parallel data already exists or ClientToken parameters changed")
		}

		if len(tx.Keys(scope.Prefix(serviceName, kindParallel))) >= 1000 {
			return invalid("at most 1000 parallel data resources per account and Region")
		}

		if e := storage.Write(tx, key, r); e != nil {
			return e
		}

		if writeErr := queue(tx, scope, kindParallel, name); writeErr != nil {
			return writeErr
		}

		return idempotency.Remember(tx, tokenKey, in, r)
	})
	return &api.CreateParallelDataResponse{Name: in.Name, Status: r.Properties.Status}, err
}

func (s *Service) UpdateParallelData(
	ctx context.Context,
	in *api.UpdateParallelDataRequest,
) (*api.UpdateParallelDataResponse, error) {
	langs, count, size, err := s.parallelInput(ctx, in.ParallelDataConfig)
	if err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.UpdateParallelDataResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := read[parallelRecord](tx, scope, kindParallel, common.Value(in.Name))
		if e != nil {
			return e
		}

		tokenKey := idempotency.Key(scope, serviceName, "UpdateParallelData", in.ClientToken)
		previous, found, replayErr := idempotency.Replay[*api.UpdateParallelDataResponse](
			tx,
			tokenKey,
			in,
			invalid("ClientToken parameters changed"),
		)
		if replayErr != nil {
			return replayErr
		}
		if found {
			out = previous
			return nil
		}

		if common.Value(r.Properties.Status) != statusActive ||
			common.Value(r.Properties.LatestUpdateAttemptStatus) == statusUpdating {
			return invalid("parallel data is not ACTIVE")
		}

		r.Properties.Description = in.Description
		r.Properties.ParallelDataConfig = in.ParallelDataConfig
		r.Properties.SourceLanguageCode = new(langs[0])
		r.Properties.TargetLanguageCodes = langs[1:]
		r.Properties.ImportedRecordCount = new(count)
		r.Properties.ImportedDataSize = new(size)
		r.Properties.LatestUpdateAttemptStatus = new(statusUpdating)
		r.Properties.LatestUpdateAttemptAt = new(now())
		if e := storage.Write(tx, scope.Key(serviceName, kindParallel, common.Value(in.Name)), r); e != nil {
			return e
		}

		if writeErr := queue(tx, scope, kindParallel, common.Value(in.Name)); writeErr != nil {
			return writeErr
		}

		out = &api.UpdateParallelDataResponse{
			Name: in.Name, Status: new(statusActive), LatestUpdateAttemptStatus: new(statusUpdating),
			LatestUpdateAttemptAt: r.Properties.LatestUpdateAttemptAt,
		}
		return idempotency.Remember(tx, tokenKey, in, out)
	})
	return out, err
}

func (s *Service) GetParallelData(
	ctx context.Context,
	in *api.GetParallelDataRequest,
) (*api.GetParallelDataResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.GetParallelDataResponse{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := read[parallelRecord](tx, scope, kindParallel, common.Value(in.Name))
		if e != nil {
			return e
		}

		out.ParallelDataProperties = new(r.Properties)
		out.DataLocation = &api.ParallelDataDataLocation{
			RepositoryType: new("S3"),
			Location:       r.Properties.ParallelDataConfig.S3Uri,
		}

		return nil
	})
	return out, err
}

func (s *Service) ListParallelData(
	ctx context.Context,
	in *api.ListParallelDataRequest,
) (*api.ListParallelDataResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	items := []api.ParallelDataProperties{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		rows, e := storage.Records[parallelRecord](tx, scope.Prefix(serviceName, kindParallel))
		if e != nil {
			return e
		}

		for _, r := range rows {
			items = append(items, r.Properties)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		items,
		func(v api.ParallelDataProperties) string { return common.Value(v.Name) },
		in.NextToken,
		limit(in.MaxResults),
		invalid("invalid NextToken"),
	)
	return &api.ListParallelDataResponse{ParallelDataPropertiesList: page, NextToken: next}, err
}

func (s *Service) DeleteParallelData(
	ctx context.Context,
	in *api.DeleteParallelDataRequest,
) (*api.DeleteParallelDataResponse, error) {
	if err := s.delete(ctx, kindParallel, common.Value(in.Name)); err != nil {
		return nil, err
	}

	return &api.DeleteParallelDataResponse{Name: in.Name, Status: new(statusDeleting)}, nil
}

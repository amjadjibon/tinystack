package polly

import (
	"context"
	"encoding/xml"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/polly"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindLexicon = "lexicon"
	maxLexicons = 100
)

type lexiconRecord struct {
	Attributes    api.LexiconAttributes
	Name, Content string
}

type pls struct {
	XMLName  xml.Name `xml:"http://www.w3.org/2005/01/pronunciation-lexicon lexicon"`
	Alphabet string   `xml:"alphabet,attr"`
	Language string   `xml:"http://www.w3.org/XML/1998/namespace lang,attr"`
	Lexemes  []lexeme `xml:"lexeme"`
}

type lexeme struct {
	Graphemes []string `xml:"grapheme"`
	Phonemes  []string `xml:"phoneme"`
	Aliases   []string `xml:"alias"`
}

func missingLexicon() error {
	return awserror.New("LexiconNotFoundException", "lexicon does not exist")
}

func parsePLS(content string) (*pls, error) {
	if len(content) > 40000 {
		return nil, awserror.New("LexiconSizeExceededException", "lexicon exceeds 40000 UTF-8 bytes")
	}

	invalid := awserror.New("InvalidLexiconException", "Content must be PLS XML with a language and complete lexemes")
	var data pls
	decoder := xml.NewDecoder(strings.NewReader(content))
	if err := decoder.Decode(&data); err != nil {
		return nil, invalid
	}

	if data.Language == "" || len(data.Lexemes) == 0 || strings.TrimSpace(content[decoder.InputOffset():]) != "" {
		return nil, invalid
	}

	if err := (&api.DescribeVoicesInput{LanguageCode: new(data.Language)}).Validate(""); err != nil {
		return nil, awserror.New("UnsupportedPlsLanguageException", "PLS language is unsupported")
	}

	switch data.Alphabet {
	case "ipa", "x-sampa":
	default:
		return nil, awserror.New("UnsupportedPlsAlphabetException", "PLS alphabet must be ipa or x-sampa")
	}

	for _, item := range data.Lexemes {
		if len(item.Graphemes) == 0 || len(item.Phonemes)+len(item.Aliases) == 0 {
			return nil, invalid
		}

		for _, values := range [][]string{item.Graphemes, item.Phonemes, item.Aliases} {
			for _, value := range values {
				if strings.TrimSpace(value) == "" {
					return nil, invalid
				}

				if len(value) > 100 {
					return nil, awserror.New("MaxLexemeLengthExceededException", "lexeme exceeds 100 UTF-8 bytes")
				}
			}
		}
	}

	return &data, nil
}

func (s *Service) PutLexicon(ctx context.Context, in *api.PutLexiconInput) (*api.PutLexiconOutput, error) {
	content := common.Value(in.Content)
	data, err := parsePLS(content)
	if err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.Name)
	key := scope.Key(serviceName, kindLexicon, name)
	arn := "arn:aws:polly:" + scope.Region + ":" + scope.AccountID + ":lexicon/" + name
	row := lexiconRecord{Name: name, Content: content, Attributes: api.LexiconAttributes{
		Alphabet: new(data.Alphabet), LanguageCode: new(data.Language), LastModified: common.SecondsMilli(time.Now()),
		LexiconArn: new(arn), LexemesCount: new(int64(len(data.Lexemes))), Size: new(int64(len(content))),
	}}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, ok := tx.Get(key); !ok && len(tx.Keys(scope.Prefix(serviceName, kindLexicon))) >= maxLexicons {
			return awserror.New("MaxLexiconsNumberExceededException", "at most 100 lexicons are supported")
		}

		return storage.Write(tx, key, row)
	})
	return &api.PutLexiconOutput{}, err
}

func (s *Service) GetLexicon(ctx context.Context, in *api.GetLexiconInput) (*api.GetLexiconOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.GetLexiconOutput{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		row, ok, e := storage.Read[lexiconRecord](tx, scope.Key(serviceName, kindLexicon, common.Value(in.Name)))
		if e != nil {
			return e
		}

		if !ok {
			return missingLexicon()
		}

		out.Lexicon = &api.Lexicon{Name: new(row.Name), Content: new(row.Content)}
		out.LexiconAttributes = &row.Attributes
		return nil
	})
	return out, err
}

func (s *Service) ListLexicons(ctx context.Context, in *api.ListLexiconsInput) (*api.ListLexiconsOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var rows []*lexiconRecord
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		rows, e = storage.Records[lexiconRecord](tx, scope.Prefix(serviceName, kindLexicon))
		return e
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		rows,
		func(row *lexiconRecord) string { return row.Name },
		in.NextToken,
		25,
		invalidToken(),
	)
	if err != nil {
		return nil, err
	}

	out := &api.ListLexiconsOutput{NextToken: next}
	for _, row := range page {
		out.Lexicons = append(out.Lexicons, api.LexiconDescription{Name: new(row.Name), Attributes: &row.Attributes})
	}

	return out, nil
}

func (s *Service) DeleteLexicon(ctx context.Context, in *api.DeleteLexiconInput) (*api.DeleteLexiconOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	key := scope.Key(serviceName, kindLexicon, common.Value(in.Name))
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, ok := tx.Get(key); !ok {
			return missingLexicon()
		}

		tx.Delete(key)
		return nil
	})
	return &api.DeleteLexiconOutput{}, err
}

// Package transcribe uses user fixtures rather than speech recognition.
package transcribe

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/transcribe"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/services/mlfixtures"
	"github.com/amjadjibon/tinystack/internal/services/s3"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName = "transcribe"
	indexPrefix = "aws/transcribe/index/"
)

type Service struct {
	store   storage.Store
	buckets *s3.Service
	workers *background.Group
}

func New(store storage.Store, buckets *s3.Service) *Service {
	return &Service{store: store, buckets: buckets, workers: background.New()}
}

func invalid(format string, args ...any) error {
	return awserror.New("BadRequestException", fmt.Sprintf(format, args...))
}

func conflict(message string) error { return awserror.New("ConflictException", message) }

func missing(name string) error {
	return awserror.New("NotFoundException", "Resource does not exist: "+name)
}

// absent reports a missing record; Transcribe answers BadRequestException for unknown jobs and vocabularies.
func absent(kind, name string) error {
	switch kind {
	case kindJob:
		return invalid("The requested job couldn't be found. Check the job name and try your request again.")
	case kindVocabulary:
		return invalid(
			"The requested vocabulary couldn't be found. Check the vocabulary name and try your request again.",
		)
	}

	return missing(name)
}

func now() float64 { return float64(time.Now().UnixMilli()) / 1000 }
func arn(scope request.Scope, kind, name string) string {
	return fmt.Sprintf("arn:aws:transcribe:%s:%s:%s/%s", scope.Region, scope.AccountID, kind, name)
}

func limit(v *int64) int {
	if v == nil {
		return 100
	}

	return int(*v)
}

type record struct {
	Job      *api.TranscriptionJob
	Tags     map[string]string
	Name     string
	Language string
	State    string
	URI      string
	Bucket   string
	Key      string
	Phrases  []string
	Modified float64
}

type work struct {
	Scope request.Scope
	Kind  string
	Name  string
}

func queue(tx storage.WriteTx, scope request.Scope, kind, name string) error {
	return storage.Write(
		tx,
		indexPrefix+scope.AccountID+"/"+scope.Region+"/"+kind+"/"+name,
		work{Scope: scope, Kind: kind, Name: name},
	)
}

func read(tx storage.ReadTx, scope request.Scope, kind, name string) (*record, error) {
	r, ok, err := storage.Read[record](tx, scope.Key(serviceName, kind, name))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, absent(kind, name)
	}

	return r, nil
}

func (s *Service) get(ctx context.Context, kind, name string) (*record, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		r, e = read(tx, scope, kind, name)
		return e
	})
	return r, err
}

func (s *Service) remove(ctx context.Context, kind, name string) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := read(tx, scope, kind, name)
		if e != nil {
			return e
		}

		if kind == kindJob && (r.State == statusQueued || r.State == statusInProgress) {
			return invalid("cannot delete an active transcription job")
		}

		tx.Delete(scope.Key(serviceName, kind, name))
		tx.Delete(indexPrefix + scope.AccountID + "/" + scope.Region + "/" + kind + "/" + name)
		return nil
	})
}

func (s *Service) list(ctx context.Context, kind, name, state string) ([]*record, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := []*record{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		rows, e := storage.Records[record](tx, scope.Prefix(serviceName, kind))
		if e != nil {
			return e
		}

		for _, r := range rows {
			if strings.Contains(r.Name, name) && (state == "" || r.State == state) {
				out = append(out, r)
			}
		}

		return nil
	})
	return out, err
}

func (s *Service) file(ctx context.Context, uri string) error {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "s3" || u.Host == "" || u.Path == "" || u.RawQuery != "" || u.Fragment != "" {
		return invalid("expected s3://bucket/key")
	}

	_, err = s.buckets.Object(ctx, u.Host, strings.TrimPrefix(u.Path, "/"), "")
	if err != nil {
		return invalid("S3 object must exist: %s", err)
	}

	return nil
}

type Fixture struct {
	Transcript   *string `json:"transcript"`
	MediaFileURI string  `json:"mediaFileUri"`
	LanguageCode string  `json:"languageCode"`
}

func Handler(store storage.Store, account string) http.Handler {
	return mlfixtures.Handler(store, account, serviceName, func(f *Fixture) error {
		u, err := url.Parse(f.MediaFileURI)
		if f.Transcript == nil || err != nil || u.Scheme != "s3" || u.Host == "" || u.Path == "" || u.RawQuery != "" ||
			u.Fragment != "" ||
			f.LanguageCode == "" {
			return fmt.Errorf("mediaFileUri (s3://bucket/key) languageCode and transcript are required")
		}

		return nil
	})
}

func (s *Service) match(ctx context.Context, scope request.Scope, uri, language string) (string, error) {
	scope.Region = ""
	var matches []*Fixture
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		rows, e := storage.Records[Fixture](tx, scope.Prefix(serviceName, "fixture"))
		if e != nil {
			return e
		}

		for _, r := range rows {
			if r.MediaFileURI == uri && r.LanguageCode == language {
				matches = append(matches, r)
			}
		}

		return nil
	})
	if err != nil {
		return "", err
	}

	if len(matches) != 1 {
		return "", invalid(
			"no unique TinyStack transcription fixture for %s (%s); register one at /_tinystack/transcribe/fixtures",
			uri,
			language,
		)
	}

	return *matches[0].Transcript, nil
}

var _ api.API = (*Service)(nil)

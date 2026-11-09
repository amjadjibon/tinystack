// Package xray stores tracing configuration and bounded trace records and computes local service graphs.
package xray

import (
	"errors"
	"fmt"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/xray"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName          = "xray"
	groupKind            = "group"
	samplingRuleKind     = "sampling-rule"
	maxGroups            = 300
	maxCustomRules       = 25
	defaultRuleName      = "Default"
	defaultRulePriority  = int64(10000)
	defaultRuleFixedRate = float64(0.05)
	defaultRuleReservoir = int64(1)
	defaultRuleVersion   = int64(1)
)

var _ api.API = (*Service)(nil)

type (
	Service struct{ store storage.Store }

	groupRecord struct {
		Group *api.Group
		Tags  map[string]string
	}

	samplingRuleRecord struct {
		Record *api.SamplingRuleRecord
		Tags   map[string]string
	}
)

func New(store storage.Store) *Service { return &Service{store: store} }

func invalid(format string, args ...any) error {
	return awserror.New("InvalidRequestException", fmt.Sprintf(format, args...))
}

func notFound(format string, args ...any) error {
	err := awserror.New("ResourceNotFoundException", fmt.Sprintf(format, args...))
	err.StatusCode = 404
	return err
}

func ruleLimitExceeded(format string, args ...any) error {
	return awserror.New("RuleLimitExceededException", fmt.Sprintf(format, args...))
}

func tagError(message string) error {
	return awserror.New("InvalidRequestException", message)
}

func checkTagLimit(err error) error {
	if errors.Is(err, tagging.ErrLimit) {
		return awserror.New("TooManyTagsException", err.Error())
	}

	return invalid("%s", err.Error())
}

func tagMap(tags []api.Tag) (map[string]string, error) {
	return tagging.Map(tags, func(t api.Tag) (*string, *string) {
		return t.Key, t.Value
	}, invalid("Tag key and value are required"), func(key string) error {
		return invalid("Duplicate tag key %s", key)
	})
}

func tagList(tags map[string]string) []api.Tag {
	return tagging.List(tags, func(k, v string) api.Tag {
		return api.Tag{Key: new(k), Value: new(v)}
	})
}

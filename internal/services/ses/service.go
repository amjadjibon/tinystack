// Package ses implements Amazon SES (API v1, the classic "email" API over AWS
// Query) on top of the SES v2 service. The two APIs share one set of
// resources in AWS, so this service keeps no records of its own: it holds a
// *sesv2.Service and turns each v1 operation into the v2 calls that read and
// write the same identities, templates, configuration sets, and send counts
// (see package sesv2), which also renders templates, checks senders,
// records messages in the delivery.Log, and publishes events.
package ses

import (
	"errors"
	"fmt"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ses"
	v2 "github.com/amjadjibon/tinystack/internal/aws/generated/sesv2"
	"github.com/amjadjibon/tinystack/internal/services/sesv2"
)

var _ api.API = (*Service)(nil)

// Service is SES API v1.
type Service struct {
	v2 *sesv2.Service
}

// New returns API v1 over the resources of the v2 service shared.
func New(shared *sesv2.Service) *Service { return &Service{v2: shared} }

func invalid(format string, args ...any) error {
	return awserror.New("InvalidParameterValue", fmt.Sprintf(format, args...))
}

// translate renders an error from the v2 service under API v1's codes:
// what was not found is named by the code, and a bad request is badRequest.
func translate(err error, badRequest string) error {
	var e *awserror.Error
	if err == nil || !errors.As(err, &e) {
		return err
	}

	switch sesv2.Missing(err) {
	case "template":
		return awserror.New("TemplateDoesNotExistException", e.Message)
	case "configuration-set":
		return awserror.New("ConfigurationSetDoesNotExistException", e.Message)
	}

	switch e.Code {
	case "BadRequestException":
		return awserror.New(badRequest, e.Message)
	case "SendingPausedException":
		return awserror.New("ConfigurationSetSendingPausedException", e.Message)
	}

	return err
}

func destination(d *api.Destination) *v2.Destination {
	if d == nil {
		return nil
	}

	return &v2.Destination{ToAddresses: d.ToAddresses, CcAddresses: d.CcAddresses, BccAddresses: d.BccAddresses}
}

func tags(in []api.MessageTag) []v2.MessageTag {
	var out []v2.MessageTag
	for _, t := range in {
		out = append(out, v2.MessageTag{Name: t.Name, Value: t.Value})
	}

	return out
}

func timestamp(seconds float64) *time.Time { return new(time.UnixMilli(int64(seconds * 1000)).UTC()) }

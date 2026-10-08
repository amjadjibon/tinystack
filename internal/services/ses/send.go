package ses

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/ses"
	v2 "github.com/amjadjibon/tinystack/internal/aws/generated/sesv2"
	"github.com/amjadjibon/tinystack/internal/common"
)

func content(c *api.Content) *v2.Content {
	if c == nil {
		return nil
	}

	return &v2.Content{Data: c.Data, Charset: c.Charset}
}

func (s *Service) SendEmail(ctx context.Context, in *api.SendEmailRequest) (*api.SendEmailResponse, error) {
	if in.Message == nil || in.Message.Subject == nil || in.Message.Body == nil {
		return nil, invalid("Message needs a Subject and a Body.")
	}

	out, err := s.v2.SendEmail(ctx, &v2.SendEmailRequest{
		FromEmailAddress: in.Source, Destination: destination(in.Destination), ReplyToAddresses: in.ReplyToAddresses,
		EmailTags: tags(in.Tags), ConfigurationSetName: in.ConfigurationSetName,
		Content: &v2.EmailContent{Simple: &v2.Message{
			Subject: content(in.Message.Subject),
			Body:    &v2.Body{Text: content(in.Message.Body.Text), Html: content(in.Message.Body.Html)},
		}},
	})
	if err != nil {
		return nil, translate(err, "InvalidParameterValue")
	}

	return &api.SendEmailResponse{MessageId: out.MessageId}, nil
}

// SendRawEmail sends a MIME message. The sender and recipients default to the
// message's From and To, Cc, and Bcc headers, as sesv2 reads them.
func (s *Service) SendRawEmail(ctx context.Context, in *api.SendRawEmailRequest) (*api.SendRawEmailResponse, error) {
	if in.RawMessage == nil || len(in.RawMessage.Data) == 0 {
		return nil, invalid("RawMessage needs Data.")
	}

	req := &v2.SendEmailRequest{
		FromEmailAddress: in.Source, EmailTags: tags(in.Tags), ConfigurationSetName: in.ConfigurationSetName,
		Content: &v2.EmailContent{Raw: &v2.RawMessage{Data: in.RawMessage.Data}},
	}
	if len(in.Destinations) > 0 {
		req.Destination = &v2.Destination{ToAddresses: in.Destinations}
	}

	out, err := s.v2.SendEmail(ctx, req)
	if err != nil {
		return nil, translate(err, "InvalidParameterValue")
	}

	return &api.SendRawEmailResponse{MessageId: out.MessageId}, nil
}

// template names the stored template a v1 request sends: its name, or its ARN
// when there is no name.
func template(name, arn, data *string) *v2.Template {
	return &v2.Template{TemplateName: name, TemplateArn: arn, TemplateData: data}
}

func (s *Service) SendTemplatedEmail(
	ctx context.Context,
	in *api.SendTemplatedEmailRequest,
) (*api.SendTemplatedEmailResponse, error) {
	out, err := s.v2.SendEmail(ctx, &v2.SendEmailRequest{
		FromEmailAddress: in.Source, Destination: destination(in.Destination), ReplyToAddresses: in.ReplyToAddresses,
		EmailTags: tags(in.Tags), ConfigurationSetName: in.ConfigurationSetName,
		Content: &v2.EmailContent{Template: template(in.Template, in.TemplateArn, in.TemplateData)},
	})
	if err != nil {
		return nil, translate(err, "InvalidParameterValue")
	}

	return &api.SendTemplatedEmailResponse{MessageId: out.MessageId}, nil
}

// bulkStatuses are API v1's names for the statuses of v2's bulk entries.
var bulkStatuses = map[string]string{"SUCCESS": "Success", "INVALID_PARAMETER": "InvalidParameterValue"}

func (s *Service) SendBulkTemplatedEmail(
	ctx context.Context,
	in *api.SendBulkTemplatedEmailRequest,
) (*api.SendBulkTemplatedEmailResponse, error) {
	req := &v2.SendBulkEmailRequest{
		FromEmailAddress: in.Source, ReplyToAddresses: in.ReplyToAddresses, DefaultEmailTags: tags(in.DefaultTags),
		ConfigurationSetName: in.ConfigurationSetName,
		DefaultContent: &v2.BulkEmailContent{
			Template: template(in.Template, in.TemplateArn, in.DefaultTemplateData),
		},
	}
	for _, d := range in.Destinations {
		req.BulkEmailEntries = append(req.BulkEmailEntries, v2.BulkEmailEntry{
			Destination: destination(d.Destination), ReplacementTags: tags(d.ReplacementTags),
			ReplacementEmailContent: &v2.ReplacementEmailContent{
				ReplacementTemplate: &v2.ReplacementTemplate{ReplacementTemplateData: d.ReplacementTemplateData},
			},
		})
	}

	out, err := s.v2.SendBulkEmail(ctx, req)
	if err != nil {
		return nil, translate(err, "InvalidParameterValue")
	}

	statuses := make([]api.BulkEmailDestinationStatus, 0, len(out.BulkEmailEntryResults))
	for _, r := range out.BulkEmailEntryResults {
		status, known := bulkStatuses[common.Value(r.Status)]
		if !known {
			status = "Failed"
		}

		statuses = append(statuses, api.BulkEmailDestinationStatus{
			Status: new(status), Error: r.Error, MessageId: r.MessageId,
		})
	}

	return &api.SendBulkTemplatedEmailResponse{Status: statuses}, nil
}

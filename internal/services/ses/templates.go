package ses

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/ses"
	v2 "github.com/amjadjibon/tinystack/internal/aws/generated/sesv2"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/sesv2"
)

const invalidTemplate = "InvalidTemplateException"

func templateContent(t *api.Template) *v2.EmailTemplateContent {
	return &v2.EmailTemplateContent{Subject: t.SubjectPart, Text: t.TextPart, Html: t.HtmlPart}
}

func (s *Service) CreateTemplate(
	ctx context.Context,
	in *api.CreateTemplateRequest,
) (*api.CreateTemplateResponse, error) {
	if in.Template == nil || in.Template.TemplateName == nil {
		return nil, invalid("Template needs a TemplateName.")
	}

	_, err := s.v2.CreateEmailTemplate(ctx, &v2.CreateEmailTemplateRequest{
		TemplateName: in.Template.TemplateName, TemplateContent: templateContent(in.Template),
	})
	if err != nil {
		return nil, translate(err, invalidTemplate)
	}

	return &api.CreateTemplateResponse{}, nil
}

func (s *Service) GetTemplate(ctx context.Context, in *api.GetTemplateRequest) (*api.GetTemplateResponse, error) {
	out, err := s.v2.GetEmailTemplate(ctx, &v2.GetEmailTemplateRequest{TemplateName: in.TemplateName})
	if err != nil {
		return nil, translate(err, invalidTemplate)
	}

	c := common.OrElse(out.TemplateContent, &v2.EmailTemplateContent{})
	return &api.GetTemplateResponse{Template: &api.Template{
		TemplateName: out.TemplateName, SubjectPart: c.Subject, TextPart: c.Text, HtmlPart: c.Html,
	}}, nil
}

func (s *Service) UpdateTemplate(
	ctx context.Context,
	in *api.UpdateTemplateRequest,
) (*api.UpdateTemplateResponse, error) {
	if in.Template == nil || in.Template.TemplateName == nil {
		return nil, invalid("Template needs a TemplateName.")
	}

	_, err := s.v2.UpdateEmailTemplate(ctx, &v2.UpdateEmailTemplateRequest{
		TemplateName: in.Template.TemplateName, TemplateContent: templateContent(in.Template),
	})
	if err != nil {
		return nil, translate(err, invalidTemplate)
	}

	return &api.UpdateTemplateResponse{}, nil
}

// DeleteTemplate deletes a template, which API v1 does whether it exists or
// not.
func (s *Service) DeleteTemplate(
	ctx context.Context,
	in *api.DeleteTemplateRequest,
) (*api.DeleteTemplateResponse, error) {
	_, err := s.v2.DeleteEmailTemplate(ctx, &v2.DeleteEmailTemplateRequest{TemplateName: in.TemplateName})
	if err != nil && sesv2.Missing(err) == "" {
		return nil, translate(err, invalidTemplate)
	}

	return &api.DeleteTemplateResponse{}, nil
}

func (s *Service) ListTemplates(
	ctx context.Context,
	in *api.ListTemplatesRequest,
) (*api.ListTemplatesResponse, error) {
	out, err := s.v2.ListEmailTemplates(
		ctx,
		&v2.ListEmailTemplatesRequest{NextToken: in.NextToken, PageSize: in.MaxItems},
	)
	if err != nil {
		return nil, translate(err, "InvalidParameterValue")
	}

	metadata := make([]api.TemplateMetadata, 0, len(out.TemplatesMetadata))
	for _, t := range out.TemplatesMetadata {
		metadata = append(metadata, api.TemplateMetadata{
			Name: t.TemplateName, CreatedTimestamp: timestamp(common.Value(t.CreatedTimestamp)),
		})
	}

	return &api.ListTemplatesResponse{TemplatesMetadata: metadata, NextToken: out.NextToken}, nil
}

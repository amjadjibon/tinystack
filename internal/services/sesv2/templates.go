package sesv2

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/textproto"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/sesv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// template is an email template's record.
type template struct {
	Tags    map[string]string
	Content api.EmailTemplateContent
	Name    string
	Created float64
}

func loadTemplate(tx storage.ReadTx, scope request.Scope, name *string) (*template, error) {
	t, ok, err := storage.Read[template](tx, scope.Key(service, kindTemplate, common.Value(name)))
	if err != nil || !ok {
		return nil, common.ErrorsOr(err, notFound(kindTemplate, "Template %s does not exist.", common.Value(name)))
	}

	return t, nil
}

// checkContent parses a template's parts, failing on what TinyStack cannot
// render.
func checkContent(c *api.EmailTemplateContent) error {
	if c == nil {
		return badRequest("TemplateContent is required.")
	}

	for part, src := range map[string]*string{"Subject": c.Subject, "Text": c.Text, "Html": c.Html} {
		if _, err := parseTemplate(common.Value(src)); err != nil {
			return badRequest("The template's %s is invalid: %v.", part, err)
		}
	}

	return nil
}

// rendered is a template rendered with its data.
type rendered struct{ Subject, Text, HTML string }

// renderContent renders a template with data, a JSON object.
func renderContent(c *api.EmailTemplateContent, data string) (*rendered, error) {
	var values any
	if data == "" {
		data = "{}"
	}

	if err := json.Unmarshal([]byte(data), &values); err != nil {
		return nil, fmt.Errorf("the template data is not JSON: %w", err)
	}

	out := &rendered{}
	for _, part := range []struct {
		src    *string
		to     *string
		escape bool
	}{{c.Subject, &out.Subject, false}, {c.Text, &out.Text, false}, {c.Html, &out.HTML, true}} {
		nodes, err := parseTemplate(common.Value(part.src))
		if err != nil {
			return nil, err
		}

		if *part.to, err = render(nodes, values, part.escape); err != nil {
			return nil, err
		}
	}

	return out, nil
}

func (s *Service) CreateEmailTemplate(
	ctx context.Context,
	in *api.CreateEmailTemplateRequest,
) (*api.CreateEmailTemplateResponse, error) {
	if err := checkName("template", in.TemplateName); err != nil {
		return nil, err
	}

	if err := checkContent(in.TemplateContent); err != nil {
		return nil, err
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	t := &template{Name: *in.TemplateName, Content: *in.TemplateContent, Tags: tags, Created: common.Unix(s.now())}
	err = s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		key := scope.Key(service, kindTemplate, t.Name)
		if _, found, readErr := storage.Read[template](tx, key); readErr != nil || found {
			return common.ErrorsOr(readErr, alreadyExists("Template %s already exists.", t.Name))
		}

		return storage.Write(tx, key, t)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateEmailTemplateResponse{}, nil
}

func (s *Service) GetEmailTemplate(
	ctx context.Context,
	in *api.GetEmailTemplateRequest,
) (*api.GetEmailTemplateResponse, error) {
	var t *template
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		t, err = loadTemplate(tx, scope, in.TemplateName)
		return err
	})
	if err != nil {
		return nil, err
	}

	return &api.GetEmailTemplateResponse{
		TemplateName: new(t.Name), TemplateContent: &t.Content, Tags: tagList(t.Tags),
	}, nil
}

func (s *Service) UpdateEmailTemplate(
	ctx context.Context,
	in *api.UpdateEmailTemplateRequest,
) (*api.UpdateEmailTemplateResponse, error) {
	if err := checkContent(in.TemplateContent); err != nil {
		return nil, err
	}

	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		t, err := loadTemplate(tx, scope, in.TemplateName)
		if err != nil {
			return err
		}

		t.Content = *in.TemplateContent
		return storage.Write(tx, scope.Key(service, kindTemplate, t.Name), t)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateEmailTemplateResponse{}, nil
}

func (s *Service) DeleteEmailTemplate(
	ctx context.Context,
	in *api.DeleteEmailTemplateRequest,
) (*api.DeleteEmailTemplateResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := loadTemplate(tx, scope, in.TemplateName); err != nil {
			return err
		}

		tx.Delete(scope.Key(service, kindTemplate, *in.TemplateName))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteEmailTemplateResponse{}, nil
}

func (s *Service) ListEmailTemplates(
	ctx context.Context,
	in *api.ListEmailTemplatesRequest,
) (*api.ListEmailTemplatesResponse, error) {
	var all []*template
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		all, err = storage.Records[template](tx, scope.Prefix(service, kindTemplate))
		return err
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(all, func(t *template) string { return t.Name }, in.NextToken, in.PageSize, 10, 100)
	if err != nil {
		return nil, err
	}

	out := &api.ListEmailTemplatesResponse{TemplatesMetadata: []api.EmailTemplateMetadata{}, NextToken: next}
	for _, t := range selected {
		out.TemplatesMetadata = append(out.TemplatesMetadata, api.EmailTemplateMetadata{
			TemplateName: new(t.Name), CreatedTimestamp: new(t.Created),
		})
	}

	return out, nil
}

func (s *Service) TestRenderEmailTemplate(
	ctx context.Context,
	in *api.TestRenderEmailTemplateRequest,
) (*api.TestRenderEmailTemplateResponse, error) {
	var t *template
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		t, err = loadTemplate(tx, scope, in.TemplateName)
		return err
	})
	if err != nil {
		return nil, err
	}

	r, err := renderContent(&t.Content, common.Value(in.TemplateData))
	if err != nil {
		return nil, badRequest("%s", err.Error())
	}

	message, err := mimeMessage(r, s.now())
	if err != nil {
		return nil, err
	}

	return &api.TestRenderEmailTemplateResponse{RenderedTemplate: new(message)}, nil
}

// mimeMessage is a rendered template as the MIME message
// TestRenderEmailTemplate returns: its text and HTML parts as alternatives.
func mimeMessage(r *rendered, now time.Time) (string, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for _, part := range []struct{ kind, content string }{{"text/plain", r.Text}, {"text/html", r.HTML}} {
		if part.content == "" {
			continue
		}

		p, err := w.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {part.kind + "; charset=UTF-8"},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return "", err
		}

		q := quotedprintable.NewWriter(p)
		if _, err = q.Write([]byte(part.content)); err != nil {
			return "", err
		}

		if closeErr := q.Close(); closeErr != nil {
			return "", closeErr
		}
	}

	if err := w.Close(); err != nil {
		return "", err
	}

	return "Date: " + now.UTC().Format(time.RFC1123Z) + "\r\n" +
		"Subject: " + mime.QEncoding.Encode("UTF-8", r.Subject) + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/alternative; boundary=\"" + w.Boundary() + "\"\r\n\r\n" + body.String(), nil
}

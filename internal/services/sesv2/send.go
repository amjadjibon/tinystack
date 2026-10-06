package sesv2

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"mime"
	"net/mail"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sesv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// maxRecipients is how many recipients one message may have, and how many
// entries a bulk send may have.
const maxRecipients = 50

func messageRejected(format string, args ...any) error {
	return awserror.New("MessageRejected", fmt.Sprintf(format, args...))
}

// email is one message SES would send.
type email struct {
	Headers  map[string]string `json:"headers,omitempty"`
	Tags     map[string]string `json:"tags,omitempty"`
	ID       string            `json:"messageId"`
	From     string            `json:"from"`
	Subject  string            `json:"subject"`
	Text     string            `json:"text,omitempty"`
	HTML     string            `json:"html,omitempty"`
	Raw      string            `json:"raw,omitempty"`
	Template string            `json:"template,omitempty"`
	// failure is why its template did not render, which means it is not sent.
	failure string
	To      []string `json:"to,omitempty"`
	Cc      []string `json:"cc,omitempty"`
	Bcc     []string `json:"bcc,omitempty"`
	ReplyTo []string `json:"replyTo,omitempty"`
}

func (e *email) recipients() []string {
	return append(append(append([]string{}, e.To...), e.Cc...), e.Bcc...)
}

// outbox is what a send committed: its messages, and where their events go.
type outbox struct {
	scope        request.Scope
	identityARN  string
	set          string
	operation    string
	destinations []api.EventDestination
	emails       []*email
}

// messageID is an ID in the form SES gives messages.
func messageID(millis int64) string {
	const hex = "0123456789abcdef"
	return fmt.Sprintf("%016x-%s-%s-%s-%s-%s-000000", millis, common.RandomID(8, hex), common.RandomID(4, hex),
		common.RandomID(4, hex), common.RandomID(4, hex), common.RandomID(12, hex))
}

// destination sets a message's recipients, which must be 1 to 50 valid
// addresses, returning what is wrong with them, if anything.
func (e *email) destination(d *api.Destination) string {
	if d != nil {
		e.To, e.Cc, e.Bcc = d.ToAddresses, d.CcAddresses, d.BccAddresses
	}

	all := e.recipients()
	if len(all) == 0 || len(all) > maxRecipients {
		return fmt.Sprintf("A message needs 1 to %d recipients.", maxRecipients)
	}

	for _, a := range all {
		if _, err := mail.ParseAddress(a); err != nil {
			return "Invalid recipient address " + a + "."
		}
	}

	return ""
}

func headerMap(headers []api.MessageHeader) map[string]string {
	if len(headers) == 0 {
		return nil
	}

	out := map[string]string{}
	for _, h := range headers {
		out[common.Value(h.Name)] = common.Value(h.Value)
	}

	return out
}

func tagsMap(tags []api.MessageTag) map[string]string {
	if len(tags) == 0 {
		return nil
	}

	out := map[string]string{}
	for _, t := range tags {
		out[common.Value(t.Name)] = common.Value(t.Value)
	}

	return out
}

// raw reads a raw MIME message's sender, recipients, and subject where the
// request leaves them out.
func (e *email) raw(data []byte) error {
	m, err := mail.ReadMessage(bytes.NewReader(data))
	if err != nil {
		return badRequest("The raw message is not a MIME message: %v.", err)
	}

	if e.From == "" {
		e.From = m.Header.Get("From")
	}

	if len(e.To)+len(e.Cc)+len(e.Bcc) == 0 {
		for _, field := range []struct {
			list   *[]string
			header string
		}{{&e.To, "To"}, {&e.Cc, "Cc"}, {&e.Bcc, "Bcc"}} {
			addresses, _ := m.Header.AddressList(field.header)
			for _, a := range addresses {
				*field.list = append(*field.list, a.String())
			}
		}
	}

	e.Subject, err = new(mime.WordDecoder).DecodeHeader(m.Header.Get("Subject"))
	if err != nil {
		e.Subject = m.Header.Get("Subject")
	}

	e.Raw = string(data)
	return nil
}

// prepare checks that from may send through the configuration set, the
// request's or else its identity's, and returns the outbox for its messages.
func prepare(tx storage.ReadTx, scope request.Scope, from string, set *string) (*outbox, error) {
	if from == "" {
		return nil, badRequest("FromEmailAddress is required.")
	}

	address, err := mail.ParseAddress(from)
	if err != nil {
		return nil, badRequest("Invalid FromEmailAddress %s.", from)
	}

	i, err := verifiedIdentity(tx, scope, address.Address)
	if err != nil {
		return nil, err
	}

	o := &outbox{scope: scope, identityARN: arn(scope, kindIdentity, i.Name), set: common.Value(set)}
	if o.set == "" {
		o.set = i.ConfigurationSet
	}

	if o.set != "" {
		c, setErr := loadConfigSet(tx, scope, &o.set)
		if setErr != nil {
			return nil, setErr
		}

		if !common.Value(common.OrElse(c.Settings.SendingOptions, &api.SendingOptions{}).SendingEnabled) {
			return nil, awserror.New("SendingPausedException",
				fmt.Sprintf("Sending is paused for configuration set %s.", o.set))
		}

		o.destinations = c.Destinations
	}

	return o, nil
}

// applyTemplate renders a template into e with its data, which replacement
// data's members override. A template that does not render leaves e's
// failure set.
func applyTemplate(tx storage.ReadTx, scope request.Scope, e *email, t *api.Template, replacement string) error {
	content := t.TemplateContent
	if content == nil {
		name := common.Value(t.TemplateName)
		if t.TemplateArn != nil {
			name = strings.TrimPrefix(*t.TemplateArn, arn(scope, kindTemplate, ""))
		}

		if name == "" {
			return badRequest("The template needs a TemplateName, TemplateArn, or TemplateContent.")
		}

		stored, err := loadTemplate(tx, scope, &name)
		if err != nil {
			return err
		}

		content, e.Template = &stored.Content, name
	}

	data, ok := mergeData(common.Value(t.TemplateData), replacement)
	if !ok {
		e.failure = "The replacement template data is not a JSON object."
		return nil
	}

	if r, renderErr := renderContent(content, data); renderErr == nil {
		e.Subject, e.Text, e.HTML = r.Subject, r.Text, r.HTML
		e.Headers = headerMap(t.Headers)
	} else {
		e.failure = renderErr.Error()
	}

	return nil
}

// mergeData overrides a template's data with the members of replacement
// data, reporting false when the replacement is not a JSON object.
func mergeData(data, replacement string) (string, bool) {
	if replacement == "" {
		return data, true
	}

	var base, over map[string]any
	_ = json.Unmarshal([]byte(data), &base)
	if json.Unmarshal([]byte(replacement), &over) != nil {
		return "", false
	}

	if base == nil {
		base = map[string]any{}
	}

	maps.Copy(base, over)
	b, err := json.Marshal(base)
	return string(b), err == nil
}

func (s *Service) SendEmail(ctx context.Context, in *api.SendEmailRequest) (*api.SendEmailResponse, error) {
	c := common.OrElse(in.Content, &api.EmailContent{})
	parts := 0
	for _, set := range []bool{c.Simple != nil, c.Raw != nil, c.Template != nil} {
		if set {
			parts++
		}
	}

	if parts != 1 {
		return nil, badRequest("Content needs exactly one of Simple, Raw, or Template.")
	}

	e := &email{
		ID: messageID(s.now().UnixMilli()), From: common.Value(in.FromEmailAddress), ReplyTo: in.ReplyToAddresses,
		Tags: tagsMap(in.EmailTags),
	}
	switch {
	case c.Simple != nil:
		if c.Simple.Subject == nil || c.Simple.Body == nil {
			return nil, badRequest("A simple message needs a Subject and a Body.")
		}

		e.Subject = common.Value(c.Simple.Subject.Data)
		e.Text = common.Value(common.OrElse(c.Simple.Body.Text, &api.Content{}).Data)
		e.HTML = common.Value(common.OrElse(c.Simple.Body.Html, &api.Content{}).Data)
		e.Headers = headerMap(c.Simple.Headers)
	case c.Raw != nil:
		if err := e.raw(c.Raw.Data); err != nil {
			return nil, err
		}
	}

	if problem := e.destination(in.Destination); problem != "" {
		return nil, badRequest("%s", problem)
	}

	var o *outbox
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		var err error
		if o, err = prepare(tx, scope, e.From, in.ConfigurationSetName); err != nil {
			return err
		}

		if c.Template != nil {
			if templateErr := applyTemplate(tx, scope, e, c.Template, ""); templateErr != nil {
				return templateErr
			}
		}

		o.operation, o.emails = "SendEmail", []*email{e}
		return s.count(tx, scope, 1)
	})
	if err != nil {
		return nil, err
	}

	s.dispatch(ctx, o)
	return &api.SendEmailResponse{MessageId: new(e.ID)}, nil
}

func (s *Service) SendBulkEmail(ctx context.Context, in *api.SendBulkEmailRequest) (*api.SendBulkEmailResponse, error) {
	if in.DefaultContent == nil || in.DefaultContent.Template == nil {
		return nil, badRequest("DefaultContent needs a Template.")
	}

	if len(in.BulkEmailEntries) == 0 || len(in.BulkEmailEntries) > maxRecipients {
		return nil, badRequest("BulkEmailEntries must have 1 to %d entries.", maxRecipients)
	}

	out := &api.SendBulkEmailResponse{}
	var o *outbox
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		var err error
		if o, err = prepare(tx, scope, common.Value(in.FromEmailAddress), in.ConfigurationSetName); err != nil {
			return err
		}

		o.operation, out.BulkEmailEntryResults = "SendBulkEmail", nil
		for _, entry := range in.BulkEmailEntries {
			e := &email{
				ID: messageID(s.now().UnixMilli()), From: *in.FromEmailAddress, ReplyTo: in.ReplyToAddresses,
				Tags: tagsMap(append(append([]api.MessageTag{}, in.DefaultEmailTags...), entry.ReplacementTags...)),
			}
			if problem := e.destination(entry.Destination); problem != "" {
				out.BulkEmailEntryResults = append(out.BulkEmailEntryResults, api.BulkEmailEntryResult{
					Status: new("INVALID_PARAMETER"), Error: new(problem),
				})
				continue
			}

			replacement := common.OrElse(
				common.OrElse(entry.ReplacementEmailContent, &api.ReplacementEmailContent{}).ReplacementTemplate,
				&api.ReplacementTemplate{},
			).ReplacementTemplateData
			templateErr := applyTemplate(tx, scope, e, in.DefaultContent.Template, common.Value(replacement))
			if templateErr != nil {
				return templateErr
			}

			if extra := headerMap(entry.ReplacementHeaders); extra != nil {
				if e.Headers == nil {
					e.Headers = map[string]string{}
				}

				maps.Copy(e.Headers, extra)
			}

			o.emails = append(o.emails, e)
			out.BulkEmailEntryResults = append(out.BulkEmailEntryResults, api.BulkEmailEntryResult{
				Status: new("SUCCESS"), MessageId: new(e.ID),
			})
		}

		return s.count(tx, scope, len(o.emails))
	})
	if err != nil {
		return nil, err
	}

	s.dispatch(ctx, o)
	return out, nil
}

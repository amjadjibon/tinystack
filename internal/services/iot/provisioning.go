package iot

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/iot"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindTemplate = "provisioning-template"
	arnTemplate  = "provisioningtemplate"
	// maxTemplateVersions is how many versions a template keeps at most.
	maxTemplateVersions = 5
	// maxTemplateBody is the largest template body IoT accepts.
	maxTemplateBody = 10 << 10
	typeFleet       = "FLEET_PROVISIONING"
	// certificateIDParameter is the parameter a provisioning request's
	// certificate ID fills.
	certificateIDParameter = "AWS::IoT::Certificate::Id"
	// The template resource types provisioning creates.
	typeThing       = "AWS::IoT::Thing"
	typeCertificate = "AWS::IoT::Certificate"
	typePolicy      = "AWS::IoT::Policy"
	// fnRef is the template function that reads a parameter.
	fnRef = "Ref"
	// keyClientID names a client in events and payloads.
	keyClientID = "clientId"
)

var templateNamePattern = regexp.MustCompile(`^[0-9A-Za-z_-]{1,36}$`)

// templateVersion is one version of a provisioning template's body.
type templateVersion struct {
	Body    string
	ID      int64
	Created float64
}

// provisioningTemplate is a provisioning template's record.
type provisioningTemplate struct {
	Hook        *api.ProvisioningHook `json:",omitempty"`
	Tags        map[string]string
	Name        string
	Description string
	RoleARN     string
	Type        string
	Versions    []templateVersion
	Created     float64
	Modified    float64
	Default     int64
	LastVersion int64
	Enabled     bool
}

func templateKey(scope request.Scope, name string) string {
	return scope.Key(service, kindTemplate, name)
}

func loadTemplate(tx storage.ReadTx, scope request.Scope, name string) (*provisioningTemplate, error) {
	t, found, err := storage.Read[provisioningTemplate](tx, templateKey(scope, name))
	if err == nil && !found {
		err = notFound("Provisioning template %s cannot be found.", name)
	}

	return t, err
}

func (t *provisioningTemplate) version(id int64) *templateVersion {
	for i := range t.Versions {
		if t.Versions[i].ID == id {
			return &t.Versions[i]
		}
	}

	return nil
}

// templateBody is a provisioning template's body.
type templateBody struct {
	Parameters          map[string]struct{ Type, Default *string }
	Mappings            map[string]map[string]map[string]any
	Resources           map[string]templateResource
	DeviceConfiguration map[string]any
	Conditions          json.RawMessage
}

type templateResource struct {
	Properties map[string]any
	Type       string
}

// resourceTypes are the template resource types provisioning creates.
var resourceTypes = []string{typeThing, typeCertificate, typePolicy}

// parseTemplateBody parses and checks a template body: its resources and the
// functions it uses.
func parseTemplateBody(text string) (*templateBody, error) {
	if len(text) > maxTemplateBody {
		return nil, invalid("The template body is larger than %d bytes.", maxTemplateBody)
	}

	var b templateBody
	if err := json.Unmarshal([]byte(text), &b); err != nil {
		return nil, invalid("The template body is not a valid provisioning template: %v.", err)
	}

	if b.Conditions != nil {
		return nil, invalid("TinyStack does not implement template Conditions.")
	}

	certificates := 0
	for name, r := range b.Resources {
		if !slices.Contains(resourceTypes, r.Type) {
			return nil, invalid("Resource %s has the unsupported type %s.", name, r.Type)
		}

		if r.Type == typeCertificate {
			certificates++
		}

		if err := checkFunctions(r.Properties); err != nil {
			return nil, invalid("Resource %s: %v", name, err)
		}
	}

	if certificates != 1 {
		return nil, invalid("A provisioning template needs exactly one AWS::IoT::Certificate resource.")
	}

	if err := checkFunctions(b.DeviceConfiguration); err != nil {
		return nil, invalid("DeviceConfiguration: %v", err)
	}

	return &b, nil
}

// templateFunctions are the intrinsic functions templates may use.
var templateFunctions = []string{fnRef, "Fn::Join", "Fn::Select", "Fn::Split", "Fn::Sub", "Fn::FindInMap"}

func checkFunctions(v any) error {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if strings.HasPrefix(k, "Fn::") && !slices.Contains(templateFunctions, k) {
				return fmt.Errorf("the function %s is not supported", k)
			}

			if err := checkFunctions(e); err != nil {
				return err
			}
		}
	case []any:
		for _, e := range x {
			if err := checkFunctions(e); err != nil {
				return err
			}
		}
	}

	return nil
}

// resolve evaluates a template value's intrinsic functions.
func (b *templateBody) resolve(v any, params map[string]string) (any, error) {
	switch x := v.(type) {
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			r, err := b.resolve(e, params)
			if err != nil {
				return nil, err
			}

			out[i] = r
		}

		return out, nil
	case map[string]any:
		if len(x) == 1 {
			for fn, arg := range x {
				if fn == fnRef || strings.HasPrefix(fn, "Fn::") {
					return b.function(fn, arg, params)
				}
			}
		}

		out := make(map[string]any, len(x))
		for k, e := range x {
			r, err := b.resolve(e, params)
			if err != nil {
				return nil, err
			}

			out[k] = r
		}

		return out, nil
	}

	return v, nil
}

func (b *templateBody) function(fn string, arg any, params map[string]string) (any, error) {
	if fn == fnRef {
		name, _ := arg.(string)
		v, ok := params[name]
		if !ok {
			return nil, fmt.Errorf("the parameter %s has no value", name)
		}

		return v, nil
	}

	if fn == "Fn::Sub" {
		text, _ := arg.(string)
		var missing error
		out := templateSub.ReplaceAllStringFunc(text, func(m string) string {
			v, ok := params[m[2:len(m)-1]]
			if !ok && missing == nil {
				missing = fmt.Errorf("the parameter %s has no value", m[2:len(m)-1])
			}

			return v
		})
		return out, missing
	}

	resolved, err := b.resolve(arg, params)
	if err != nil {
		return nil, err
	}

	args, _ := resolved.([]any)
	text := func(i int) string {
		if i < len(args) {
			return stringOf(args[i])
		}

		return ""
	}

	switch fn {
	case "Fn::Join":
		list, _ := argAt(args, 1).([]any)
		parts := make([]string, len(list))
		for i, e := range list {
			parts[i] = stringOf(e)
		}

		return strings.Join(parts, text(0)), nil
	case "Fn::Select":
		i, err := strconv.Atoi(text(0))
		list, _ := argAt(args, 1).([]any)
		if err != nil || i < 0 || i >= len(list) {
			return nil, fmt.Errorf("Fn::Select's index %s is out of range", text(0))
		}

		return list[i], nil
	case "Fn::Split":
		parts := strings.Split(text(1), text(0))
		out := make([]any, len(parts))
		for i, p := range parts {
			out[i] = p
		}

		return out, nil
	case "Fn::FindInMap":
		v, ok := b.Mappings[text(0)][text(1)][text(2)]
		if !ok {
			return nil, fmt.Errorf("Fn::FindInMap found nothing at %s.%s.%s", text(0), text(1), text(2))
		}

		return v, nil
	}

	return nil, fmt.Errorf("the function %s is not supported", fn)
}

var templateSub = regexp.MustCompile(`\$\{[^}]+\}`)

func argAt(args []any, i int) any {
	if i < len(args) {
		return args[i]
	}

	return nil
}

func (t *provisioningTemplate) checkSettings() error {
	switch {
	case !rolePattern.MatchString(t.RoleARN):
		return invalid("The provisioning role %s is not an IAM role ARN.", t.RoleARN)
	case t.Type != typeFleet:
		return invalid("Only FLEET_PROVISIONING templates are supported; just-in-time provisioning is not.")
	case t.Hook != nil && !strings.HasPrefix(common.Value(t.Hook.TargetArn), "arn:aws:lambda:"):
		return invalid("The pre-provisioning hook must be a Lambda function ARN.")
	case t.Hook != nil && common.Value(t.Hook.PayloadVersion) != "" &&
		common.Value(t.Hook.PayloadVersion) != "2020-04-01":
		return invalid("The pre-provisioning hook payload version must be 2020-04-01.")
	}

	return nil
}

func (s *Service) CreateProvisioningTemplate(
	ctx context.Context,
	in *api.CreateProvisioningTemplateRequest,
) (*api.CreateProvisioningTemplateResponse, error) {
	name := common.Value(in.TemplateName)
	if !templateNamePattern.MatchString(name) {
		return nil, invalid("The template name must be 1 to 36 letters, digits, underscores, or hyphens.")
	}

	if _, err := parseTemplateBody(common.Value(in.TemplateBody)); err != nil {
		return nil, err
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	now := common.Unix(s.now())
	t := &provisioningTemplate{
		Name: name, Description: common.Value(in.Description), RoleARN: common.Value(in.ProvisioningRoleArn),
		Hook: in.PreProvisioningHook, Type: common.Value(common.OrElse(in.Type, new(typeFleet))), Tags: tags,
		Enabled: common.Value(in.Enabled), Created: now, Modified: now, Default: 1, LastVersion: 1,
		Versions: []templateVersion{{ID: 1, Body: *in.TemplateBody, Created: now}},
	}
	if checkErr := t.checkSettings(); checkErr != nil {
		return nil, checkErr
	}

	var out *api.CreateProvisioningTemplateResponse
	err = s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if exists(tx, templateKey(scope, name)) {
			return alreadyExists("Provisioning template %s already exists.", name)
		}

		out = &api.CreateProvisioningTemplateResponse{
			TemplateArn: new(arn(scope, arnTemplate, name)), TemplateName: new(name), DefaultVersionId: new(int64(1)),
		}
		return storage.Write(tx, templateKey(scope, name), t)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DescribeProvisioningTemplate(
	ctx context.Context,
	in *api.DescribeProvisioningTemplateRequest,
) (*api.DescribeProvisioningTemplateResponse, error) {
	var out *api.DescribeProvisioningTemplateResponse
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		t, err := loadTemplate(tx, scope, common.Value(in.TemplateName))
		if err != nil {
			return err
		}

		out = &api.DescribeProvisioningTemplateResponse{
			TemplateArn: new(arn(scope, arnTemplate, t.Name)), TemplateName: new(t.Name),
			Description: new(t.Description), CreationDate: new(t.Created), LastModifiedDate: new(t.Modified),
			DefaultVersionId: new(t.Default), TemplateBody: new(t.version(t.Default).Body), Enabled: new(t.Enabled),
			ProvisioningRoleArn: new(t.RoleARN), PreProvisioningHook: t.Hook, Type: new(t.Type),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) UpdateProvisioningTemplate(
	ctx context.Context,
	in *api.UpdateProvisioningTemplateRequest,
) (*api.UpdateProvisioningTemplateResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		t, err := loadTemplate(tx, scope, common.Value(in.TemplateName))
		if err != nil {
			return err
		}

		if in.DefaultVersionId != nil {
			if t.version(*in.DefaultVersionId) == nil {
				return notFound("Version %d of template %s cannot be found.", *in.DefaultVersionId, t.Name)
			}

			t.Default = *in.DefaultVersionId
		}

		t.Description = common.Value(common.OrElse(in.Description, &t.Description))
		t.Enabled = common.Value(common.OrElse(in.Enabled, &t.Enabled))
		t.RoleARN = common.Value(common.OrElse(in.ProvisioningRoleArn, &t.RoleARN))
		switch {
		case common.Value(in.RemovePreProvisioningHook) && in.PreProvisioningHook != nil:
			return invalid("Set preProvisioningHook or removePreProvisioningHook, not both.")
		case common.Value(in.RemovePreProvisioningHook):
			t.Hook = nil
		case in.PreProvisioningHook != nil:
			t.Hook = in.PreProvisioningHook
		}

		if err := t.checkSettings(); err != nil {
			return err
		}

		t.Modified = common.Unix(s.now())
		return storage.Write(tx, templateKey(scope, t.Name), t)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateProvisioningTemplateResponse{}, nil
}

func (s *Service) ListProvisioningTemplates(
	ctx context.Context,
	in *api.ListProvisioningTemplatesRequest,
) (*api.ListProvisioningTemplatesResponse, error) {
	var out []api.ProvisioningTemplateSummary
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		templates, err := storage.Records[provisioningTemplate](tx, scope.Prefix(service, kindTemplate))
		for _, t := range templates {
			out = append(out, api.ProvisioningTemplateSummary{
				TemplateArn: new(arn(scope, arnTemplate, t.Name)), TemplateName: new(t.Name),
				Description: new(t.Description), CreationDate: new(t.Created), LastModifiedDate: new(t.Modified),
				Enabled: new(t.Enabled), Type: new(t.Type),
			})
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(out, func(t api.ProvisioningTemplateSummary) string { return *t.TemplateName },
		in.NextToken, in.MaxResults)
	if err != nil {
		return nil, err
	}

	if selected == nil {
		selected = []api.ProvisioningTemplateSummary{}
	}

	return &api.ListProvisioningTemplatesResponse{Templates: selected, NextToken: next}, nil
}

func (s *Service) DeleteProvisioningTemplate(
	ctx context.Context,
	in *api.DeleteProvisioningTemplateRequest,
) (*api.DeleteProvisioningTemplateResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := loadTemplate(tx, scope, common.Value(in.TemplateName)); err != nil {
			return err
		}

		tx.Delete(templateKey(scope, common.Value(in.TemplateName)))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteProvisioningTemplateResponse{}, nil
}

func (s *Service) CreateProvisioningTemplateVersion(
	ctx context.Context,
	in *api.CreateProvisioningTemplateVersionRequest,
) (*api.CreateProvisioningTemplateVersionResponse, error) {
	if _, err := parseTemplateBody(common.Value(in.TemplateBody)); err != nil {
		return nil, err
	}

	var out *api.CreateProvisioningTemplateVersionResponse
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		t, err := loadTemplate(tx, scope, common.Value(in.TemplateName))
		if err != nil {
			return err
		}

		if len(t.Versions) >= maxTemplateVersions {
			return awserror.New("VersionsLimitExceededException",
				fmt.Sprintf("Template %s already has %d versions.", t.Name, maxTemplateVersions))
		}

		t.LastVersion++
		now := common.Unix(s.now())
		t.Versions = append(t.Versions, templateVersion{ID: t.LastVersion, Body: *in.TemplateBody, Created: now})
		if common.Value(in.SetAsDefault) {
			t.Default = t.LastVersion
		}

		t.Modified = now
		out = &api.CreateProvisioningTemplateVersionResponse{
			TemplateArn: new(arn(scope, arnTemplate, t.Name)), TemplateName: new(t.Name),
			VersionId: new(t.LastVersion), IsDefaultVersion: new(t.Default == t.LastVersion),
		}
		return storage.Write(tx, templateKey(scope, t.Name), t)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DescribeProvisioningTemplateVersion(
	ctx context.Context,
	in *api.DescribeProvisioningTemplateVersionRequest,
) (*api.DescribeProvisioningTemplateVersionResponse, error) {
	var out *api.DescribeProvisioningTemplateVersionResponse
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		t, err := loadTemplate(tx, scope, common.Value(in.TemplateName))
		if err != nil {
			return err
		}

		v := t.version(common.Value(in.VersionId))
		if v == nil {
			return notFound("Version %d of template %s cannot be found.", common.Value(in.VersionId), t.Name)
		}

		out = &api.DescribeProvisioningTemplateVersionResponse{
			VersionId: new(v.ID), CreationDate: new(v.Created), TemplateBody: new(v.Body),
			IsDefaultVersion: new(v.ID == t.Default),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) ListProvisioningTemplateVersions(
	ctx context.Context,
	in *api.ListProvisioningTemplateVersionsRequest,
) (*api.ListProvisioningTemplateVersionsResponse, error) {
	var out []api.ProvisioningTemplateVersionSummary
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		t, err := loadTemplate(tx, scope, common.Value(in.TemplateName))
		if err != nil {
			return err
		}

		for _, v := range t.Versions {
			out = append(out, api.ProvisioningTemplateVersionSummary{
				VersionId: new(v.ID), CreationDate: new(v.Created), IsDefaultVersion: new(v.ID == t.Default),
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(out, func(v api.ProvisioningTemplateVersionSummary) string {
		return fmt.Sprintf("%010d", *v.VersionId)
	}, in.NextToken, in.MaxResults)
	if err != nil {
		return nil, err
	}

	if selected == nil {
		selected = []api.ProvisioningTemplateVersionSummary{}
	}

	return &api.ListProvisioningTemplateVersionsResponse{Versions: selected, NextToken: next}, nil
}

func (s *Service) DeleteProvisioningTemplateVersion(
	ctx context.Context,
	in *api.DeleteProvisioningTemplateVersionRequest,
) (*api.DeleteProvisioningTemplateVersionResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		t, err := loadTemplate(tx, scope, common.Value(in.TemplateName))
		if err != nil {
			return err
		}

		id := common.Value(in.VersionId)
		switch {
		case t.version(id) == nil:
			return notFound("Version %d of template %s cannot be found.", id, t.Name)
		case id == t.Default:
			return conflict("Version %d is the default version of template %s.", id, t.Name)
		}

		t.Versions = slices.DeleteFunc(t.Versions, func(v templateVersion) bool { return v.ID == id })
		t.Modified = common.Unix(s.now())
		return storage.Write(tx, templateKey(scope, t.Name), t)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteProvisioningTemplateVersionResponse{}, nil
}

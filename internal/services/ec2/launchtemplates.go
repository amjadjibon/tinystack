package ec2

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindLaunchTemplate    = "launch-template"
	versionLatest         = "$Latest"
	versionDefault        = "$Default"
	maxTemplateVersions   = 10000
	maxDescribeTemplates  = 200
	maxVersionDescription = 255
)

var templateNamePattern = regexp.MustCompile(`^[a-zA-Z0-9().\-/_]{3,128}$`)

// launchTemplate is a launch template with all of its versions, oldest
// first. Version numbers are never reused.
type launchTemplate struct {
	CreatedAt time.Time
	Tags      map[string]string `json:",omitempty"`
	ID        string
	Name      string
	CreatedBy string
	Versions  []templateVersion
	Default   int64
	Latest    int64
}

type templateVersion struct {
	CreatedAt   time.Time
	Data        *api.ResponseLaunchTemplateData
	Description string `json:",omitempty"`
	Number      int64
}

func templateIDNotFound(id string) error {
	return awserror.New("InvalidLaunchTemplateId.NotFound",
		fmt.Sprintf("The specified launch template, with template ID %s, does not exist.", id))
}

func templateNameNotFound(name string) error {
	return awserror.New("InvalidLaunchTemplateName.NotFoundException",
		fmt.Sprintf("The specified launch template, with template name %s, does not exist.", name))
}

func templateOutput(t *launchTemplate) *api.LaunchTemplate {
	return &api.LaunchTemplate{
		LaunchTemplateId: new(t.ID), LaunchTemplateName: new(t.Name), CreateTime: new(t.CreatedAt),
		CreatedBy: new(t.CreatedBy), DefaultVersionNumber: new(t.Default), LatestVersionNumber: new(t.Latest),
		Tags: tagList(t.Tags),
	}
}

func versionOutput(t *launchTemplate, v *templateVersion) api.LaunchTemplateVersion {
	return api.LaunchTemplateVersion{
		LaunchTemplateId: new(t.ID), LaunchTemplateName: new(t.Name), VersionNumber: new(v.Number),
		VersionDescription: optional(v.Description), CreateTime: new(v.CreatedAt), CreatedBy: new(t.CreatedBy),
		DefaultVersion: new(v.Number == t.Default), LaunchTemplateData: v.Data,
	}
}

func templateValues(t *launchTemplate) values {
	return values{
		"launch-template-name": {t.Name}, "create-time": {t.CreatedAt.Format(time.RFC3339)},
	}.withTags(t.Tags)
}

func versionValues(t *launchTemplate, v *templateVersion) values {
	d := v.Data
	return values{
		filterImageID: {common.Value(d.ImageId)}, "instance-type": {common.Value(d.InstanceType)},
		"is-default-version": boolValue(v.Number == t.Default), "version-description": {v.Description},
	}
}

// templateData checks a launch template's data and returns it as EC2
// reports it. The request and response shapes have the same members.
func templateData(in *api.RequestLaunchTemplateData, base *api.ResponseLaunchTemplateData) (
	*api.ResponseLaunchTemplateData, error,
) {
	if in == nil {
		return nil, awserror.New("MissingParameter", "The request must contain the parameter LaunchTemplateData")
	}

	if t := common.Value(in.InstanceType); t != "" {
		if _, err := instanceType(t); err != nil {
			return nil, err
		}
	}

	if b := common.Value(in.InstanceInitiatedShutdownBehavior); b != "" && b != shutdownStop && b != shutdownTerminate {
		return nil, invalidParameter(fmt.Sprintf(
			"Value (%s) for parameter instanceInitiatedShutdownBehavior is invalid.", b))
	}

	if u := common.Value(in.UserData); u != "" {
		if _, err := base64.StdEncoding.DecodeString(u); err != nil {
			return nil, invalidParameter("Invalid BASE64 encoding of user data.")
		}
	}

	for _, spec := range in.TagSpecifications {
		if _, err := tagMap(spec.Tags); err != nil {
			return nil, err
		}
	}

	// A new version based on another overrides the members it sets.
	out := &api.ResponseLaunchTemplateData{}
	return out, overlay(base, in, out)
}

// overlay decodes into out the members of base (when not nil) that top does
// not set, and then those top sets, dropping base's members named in drop.
func overlay(base, top, out any, drop ...string) error {
	merged := map[string]json.RawMessage{}
	for _, v := range []any{base, top} {
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}

		if decodeErr := json.Unmarshal(data, &merged); decodeErr != nil {
			return decodeErr
		}

		for _, k := range drop {
			delete(merged, k)
		}

		drop = nil
	}

	data, err := json.Marshal(merged)
	if err != nil {
		return err
	}

	return json.Unmarshal(data, out)
}

// findTemplate returns the launch template an ID or name names.
func findTemplate(tx storage.ReadTx, scope request.Scope, id, name string) (*launchTemplate, error) {
	switch {
	case id != "" && name != "":
		return nil, awserror.New("InvalidParameterCombination",
			"The parameters LaunchTemplateId and LaunchTemplateName cannot be used together")
	case id != "":
		t, ok, err := storage.Read[launchTemplate](tx, scope.Key("ec2", kindLaunchTemplate, id))
		if err == nil && !ok {
			err = templateIDNotFound(id)
		}

		return t, err
	case name != "":
		all, err := list[launchTemplate](tx, scope, kindLaunchTemplate)
		if err != nil {
			return nil, err
		}

		if i := slices.IndexFunc(all, func(t *launchTemplate) bool { return t.Name == name }); i >= 0 {
			return all[i], nil
		}

		return nil, templateNameNotFound(name)
	}

	return nil, awserror.New("MissingParameter", "The request must contain the parameter LaunchTemplateId or "+
		"LaunchTemplateName")
}

// version returns the version a version string names: a number, $Latest,
// or $Default.
func (t *launchTemplate) version(s string) (*templateVersion, error) {
	n := t.Default
	switch s {
	case versionLatest:
		n = t.Latest
	case versionDefault, "":
	default:
		parsed, err := strconv.ParseInt(s, 10, 64)
		if err != nil || parsed < 1 {
			return nil, awserror.New("InvalidLaunchTemplateId.VersionNotFound",
				fmt.Sprintf("Could not find launch template version %s for launch template %s.", s, t.ID))
		}

		n = parsed
	}

	for i := range t.Versions {
		if t.Versions[i].Number == n {
			return &t.Versions[i], nil
		}
	}

	return nil, awserror.New("InvalidLaunchTemplateId.VersionNotFound",
		fmt.Sprintf("Could not find launch template version %s for launch template %s.", s, t.ID))
}

func checkVersionDescription(d *string) error {
	if len(common.Value(d)) > maxVersionDescription {
		return invalidParameter("The version description must be at most 255 characters long.")
	}

	return nil
}

func (s *Service) CreateLaunchTemplate(
	ctx context.Context,
	in *api.CreateLaunchTemplateRequest,
) (*api.CreateLaunchTemplateResult, error) {
	m, err := caller(ctx, in.DryRun)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.LaunchTemplateName)
	if !templateNamePattern.MatchString(name) {
		return nil, awserror.New("InvalidLaunchTemplateName.MalformedException",
			"A launch template name must be between 3 and 128 characters, and may contain letters, numbers, and "+
				"the following characters: - ( ) . / _.")
	}

	if checkErr := checkVersionDescription(in.VersionDescription); checkErr != nil {
		return nil, checkErr
	}

	data, err := templateData(in.LaunchTemplateData, nil)
	if err != nil {
		return nil, err
	}

	tags, err := specTags(in.TagSpecifications, kindLaunchTemplate)
	if err != nil {
		return nil, err
	}

	now := s.now().UTC()
	t := &launchTemplate{
		ID:        newID("lt"),
		Name:      name,
		CreatedAt: now,
		CreatedBy: "arn:aws:iam::" + m.AccountID + ":root",
		Default:   1,
		Latest:    1,
		Tags:      tags,
		Versions: []templateVersion{
			{Number: 1, Description: common.Value(in.VersionDescription), CreatedAt: now, Data: data},
		},
	}
	err = s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		token := idempotency.Key(scope, "ec2", "CreateLaunchTemplate", in.ClientToken)
		previous, found, replayErr := idempotency.Replay[*launchTemplate](tx, token, in,
			awserror.New("IdempotentParameterMismatch", "ClientToken parameters differ"))
		if replayErr != nil || found {
			t = previous
			return replayErr
		}

		if _, findErr := findTemplate(tx, scope, "", name); findErr == nil {
			return awserror.New("InvalidLaunchTemplateName.AlreadyExistsException",
				"Launch template name already in use.")
		}

		if e := storage.Write(tx, scope.Key("ec2", kindLaunchTemplate, t.ID), t); e != nil {
			return e
		}

		return idempotency.Remember(tx, token, in, t)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateLaunchTemplateResult{LaunchTemplate: templateOutput(t)}, nil
}

func (s *Service) CreateLaunchTemplateVersion(
	ctx context.Context,
	in *api.CreateLaunchTemplateVersionRequest,
) (*api.CreateLaunchTemplateVersionResult, error) {
	if err := checkVersionDescription(in.VersionDescription); err != nil {
		return nil, err
	}

	var out api.LaunchTemplateVersion
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		token := idempotency.Key(scope, "ec2", "CreateLaunchTemplateVersion", in.ClientToken)
		previous, found, replayErr := idempotency.Replay[api.LaunchTemplateVersion](tx, token, in,
			awserror.New("IdempotentParameterMismatch", "ClientToken parameters differ"))
		if replayErr != nil || found {
			out = previous
			return replayErr
		}

		changeErr := func() error {
			t, err := findTemplate(tx, scope, common.Value(in.LaunchTemplateId), common.Value(in.LaunchTemplateName))
			if err != nil {
				return err
			}

			if len(t.Versions) >= maxTemplateVersions {
				return awserror.New("VersionLimitExceeded", "A launch template can have at most 10000 versions.")
			}

			var base *api.ResponseLaunchTemplateData
			if in.SourceVersion != nil {
				source, versionErr := t.version(*in.SourceVersion)
				if versionErr != nil {
					return versionErr
				}

				base = source.Data
			}

			data, err := templateData(in.LaunchTemplateData, base)
			if err != nil {
				return err
			}

			t.Latest++
			t.Versions = append(t.Versions, templateVersion{
				Number: t.Latest, Description: common.Value(in.VersionDescription),
				CreatedAt: s.now().UTC(), Data: data,
			})
			out = versionOutput(t, &t.Versions[len(t.Versions)-1])
			return storage.Write(tx, scope.Key("ec2", kindLaunchTemplate, t.ID), t)
		}()
		if changeErr != nil {
			return changeErr
		}

		return idempotency.Remember(tx, token, in, out)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateLaunchTemplateVersionResult{LaunchTemplateVersion: &out}, nil
}

func (s *Service) DescribeLaunchTemplates(
	ctx context.Context,
	in *api.DescribeLaunchTemplatesRequest,
) (*api.DescribeLaunchTemplatesResult, error) {
	match, err := compileFilters(in.Filters, templateValues(&launchTemplate{}).names()...)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeLaunchTemplatesResult{LaunchTemplates: []api.LaunchTemplate{}}
	err = s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		all, listErr := list[launchTemplate](tx, scope, kindLaunchTemplate)
		if listErr != nil {
			return listErr
		}

		selected, selectErr := selectIDs(all, func(t *launchTemplate) string { return t.ID }, in.LaunchTemplateIds,
			templateIDNotFound)
		if selectErr != nil {
			return selectErr
		}

		selected, selectErr = selectIDs(selected, func(t *launchTemplate) string { return t.Name },
			in.LaunchTemplateNames, templateNameNotFound)
		if selectErr != nil {
			return selectErr
		}

		selected = slices.DeleteFunc(selected, func(t *launchTemplate) bool { return !match.match(templateValues(t)) })
		selected, next, pageErr := page(selected, func(t *launchTemplate) string { return t.ID }, in.NextToken,
			in.MaxResults, maxDescribeTemplates)
		if pageErr != nil {
			return pageErr
		}

		for _, t := range selected {
			out.LaunchTemplates = append(out.LaunchTemplates, *templateOutput(t))
		}

		out.NextToken = next
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// DescribeLaunchTemplateVersions lists one template's versions, newest
// first, or the versions Versions names (numbers, $Latest, $Default), in
// that order.
func (s *Service) DescribeLaunchTemplateVersions(
	ctx context.Context,
	in *api.DescribeLaunchTemplateVersionsRequest,
) (*api.DescribeLaunchTemplateVersionsResult, error) {
	match, err := compileFilters(in.Filters, versionValues(&launchTemplate{},
		&templateVersion{Data: &api.ResponseLaunchTemplateData{}}).names()...)
	if err != nil {
		return nil, err
	}

	minVersion, maxVersion := int64(0), int64(maxTemplateVersions)
	for _, bound := range []struct {
		given *string
		to    *int64
	}{{in.MinVersion, &minVersion}, {in.MaxVersion, &maxVersion}} {
		if bound.given != nil {
			if *bound.to, err = strconv.ParseInt(*bound.given, 10, 64); err != nil {
				return nil, invalidParameter("MinVersion and MaxVersion must be version numbers.")
			}
		}
	}

	out := &api.DescribeLaunchTemplateVersionsResult{LaunchTemplateVersions: []api.LaunchTemplateVersion{}}
	err = s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		t, findErr := findTemplate(tx, scope, common.Value(in.LaunchTemplateId), common.Value(in.LaunchTemplateName))
		if findErr != nil {
			return findErr
		}

		var selected []*templateVersion
		if len(in.Versions) > 0 {
			for _, want := range in.Versions {
				v, versionErr := t.version(want)
				if versionErr != nil {
					return versionErr
				}

				selected = append(selected, v)
			}
		} else {
			for i := len(t.Versions) - 1; i >= 0; i-- {
				selected = append(selected, &t.Versions[i])
			}
		}

		// Pages are keyed by a version number padded so they sort newest first.
		sortKey := func(v *templateVersion) string { return fmt.Sprintf("%05d", maxTemplateVersions-v.Number) }
		selected = slices.DeleteFunc(selected, func(v *templateVersion) bool {
			return v.Number < minVersion || v.Number > maxVersion || !match.match(versionValues(t, v))
		})
		if len(in.Versions) == 0 {
			var next *string
			selected, next, err = page(selected, sortKey, in.NextToken, in.MaxResults, maxDescribeTemplates)
			if err != nil {
				return err
			}

			out.NextToken = next
		}

		for _, v := range selected {
			out.LaunchTemplateVersions = append(out.LaunchTemplateVersions, versionOutput(t, v))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) ModifyLaunchTemplate(
	ctx context.Context,
	in *api.ModifyLaunchTemplateRequest,
) (*api.ModifyLaunchTemplateResult, error) {
	var out *api.LaunchTemplate
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		token := idempotency.Key(scope, "ec2", "ModifyLaunchTemplate", in.ClientToken)
		previous, found, replayErr := idempotency.Replay[*api.LaunchTemplate](tx, token, in,
			awserror.New("IdempotentParameterMismatch", "ClientToken parameters differ"))
		if replayErr != nil || found {
			out = previous
			return replayErr
		}

		changeErr := func() error {
			t, err := findTemplate(tx, scope, common.Value(in.LaunchTemplateId), common.Value(in.LaunchTemplateName))
			if err != nil {
				return err
			}

			if in.DefaultVersion != nil {
				v, versionErr := t.version(*in.DefaultVersion)
				if versionErr != nil {
					return versionErr
				}

				t.Default = v.Number
			}

			out = templateOutput(t)
			return storage.Write(tx, scope.Key("ec2", kindLaunchTemplate, t.ID), t)
		}()
		if changeErr != nil {
			return changeErr
		}

		return idempotency.Remember(tx, token, in, out)
	})
	if err != nil {
		return nil, err
	}

	return &api.ModifyLaunchTemplateResult{LaunchTemplate: out}, nil
}

func (s *Service) DeleteLaunchTemplate(
	ctx context.Context,
	in *api.DeleteLaunchTemplateRequest,
) (*api.DeleteLaunchTemplateResult, error) {
	var out *api.LaunchTemplate
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		t, err := findTemplate(tx, scope, common.Value(in.LaunchTemplateId), common.Value(in.LaunchTemplateName))
		if err != nil {
			return err
		}

		out = templateOutput(t)
		tx.Delete(scope.Key("ec2", kindLaunchTemplate, t.ID))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteLaunchTemplateResult{LaunchTemplate: out}, nil
}

// DeleteLaunchTemplateVersions deletes versions other than the default one,
// reporting each version it could not delete.
func (s *Service) DeleteLaunchTemplateVersions(
	ctx context.Context,
	in *api.DeleteLaunchTemplateVersionsRequest,
) (*api.DeleteLaunchTemplateVersionsResult, error) {
	out := &api.DeleteLaunchTemplateVersionsResult{
		SuccessfullyDeletedLaunchTemplateVersions:   []api.DeleteLaunchTemplateVersionsResponseSuccessItem{},
		UnsuccessfullyDeletedLaunchTemplateVersions: []api.DeleteLaunchTemplateVersionsResponseErrorItem{},
	}
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		t, err := findTemplate(tx, scope, common.Value(in.LaunchTemplateId), common.Value(in.LaunchTemplateName))
		if err != nil {
			return err
		}

		for _, want := range in.Versions {
			n, parseErr := strconv.ParseInt(want, 10, 64)
			i := slices.IndexFunc(t.Versions, func(v templateVersion) bool { return v.Number == n })
			failure := func(code, message string) {
				out.UnsuccessfullyDeletedLaunchTemplateVersions = append(
					out.UnsuccessfullyDeletedLaunchTemplateVersions, api.DeleteLaunchTemplateVersionsResponseErrorItem{
						LaunchTemplateId: new(t.ID), LaunchTemplateName: new(t.Name), VersionNumber: new(n),
						ResponseError: &api.ResponseError{Code: new(code), Message: new(message)},
					})
			}

			switch {
			case parseErr != nil || i < 0:
				failure("launchTemplateVersionDoesNotExist", "The specified launch template version does not exist.")
			case n == t.Default:
				failure("unexpectedError", "Cannot delete the default version of a launch template.")
			default:
				t.Versions = slices.Delete(t.Versions, i, i+1)
				out.SuccessfullyDeletedLaunchTemplateVersions = append(out.SuccessfullyDeletedLaunchTemplateVersions,
					api.DeleteLaunchTemplateVersionsResponseSuccessItem{
						LaunchTemplateId: new(t.ID), LaunchTemplateName: new(t.Name), VersionNumber: new(n),
					})
			}
		}

		return storage.Write(tx, scope.Key("ec2", kindLaunchTemplate, t.ID), t)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// withTemplate returns a RunInstances request with the launch template it
// names applied: members the request sets win, and the template's instance
// tags come before the request's.
func withTemplate(tx storage.ReadTx, scope request.Scope, in *api.RunInstancesRequest) (
	*api.RunInstancesRequest, error,
) {
	spec := in.LaunchTemplate
	if spec == nil {
		return in, nil
	}

	t, err := findTemplate(tx, scope, common.Value(spec.LaunchTemplateId), common.Value(spec.LaunchTemplateName))
	if err != nil {
		return nil, err
	}

	v, err := t.version(common.Value(spec.Version))
	if err != nil {
		return nil, err
	}

	// RunInstances does not take EbsOptimized, and instance tags are merged
	// below.
	out := &api.RunInstancesRequest{}
	err = overlay(v.Data, in, out, "EbsOptimized", "TagSpecifications")
	if err != nil {
		return nil, err
	}

	out.TagSpecifications = nil
	for _, spec := range v.Data.TagSpecifications {
		if common.Value(spec.ResourceType) == kindInstance {
			out.TagSpecifications = append(out.TagSpecifications, api.TagSpecification(spec))
		}
	}

	out.TagSpecifications = append(out.TagSpecifications, in.TagSpecifications...)
	out.LaunchTemplate = nil
	return out, nil
}

// launchTemplateRequest reads the launch template a RunInstances request
// names, outside the launch's own transaction.
func (s *Service) launchTemplateRequest(ctx context.Context, in *api.RunInstancesRequest) (
	*api.RunInstancesRequest, error,
) {
	if in.LaunchTemplate == nil {
		return in, nil
	}

	var out *api.RunInstancesRequest
	err := s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		out, err = withTemplate(tx, scope, in)
		return err
	})

	return out, err
}

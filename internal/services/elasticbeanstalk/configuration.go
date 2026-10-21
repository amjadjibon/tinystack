package elasticbeanstalk

import (
	"context"
	"slices"
	"strconv"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/elasticbeanstalk"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type option struct {
	namespace, name, def, kind string
	values                     []string
}

var options = []option{
	{
		"aws:elasticbeanstalk:environment",
		"EnvironmentType",
		"LoadBalanced",
		scalar,
		[]string{"SingleInstance", "LoadBalanced"},
	},
	{"aws:elasticbeanstalk:environment", "ServiceRole", "", scalar, nil},
	{"aws:autoscaling:launchconfiguration", "InstanceType", "t3.micro", scalar, nil},
	{"aws:autoscaling:launchconfiguration", "IamInstanceProfile", "", scalar, nil},
	{scalingNamespace, "MinSize", "1", scalar, nil},
	{scalingNamespace, "MaxSize", "4", scalar, nil},
	{"aws:elasticbeanstalk:healthreporting:system", "SystemType", "basic", scalar, []string{"basic", "enhanced"}},
	{"aws:elasticbeanstalk:application:environment", "*", "", scalar, nil},
}

func checkOption(v api.ConfigurationOptionSetting) error {
	if v.ResourceName != nil {
		return invalid("ResourceName is not supported.")
	}

	if common.Value(v.OptionName) == "" || v.Value == nil {
		return invalid("OptionName and Value required.")
	}

	for _, o := range options {
		if common.Value(v.Namespace) != o.namespace || (o.name != "*" && common.Value(v.OptionName) != o.name) {
			continue
		}

		if len(o.values) > 0 && !slices.Contains(o.values, *v.Value) {
			return invalid("Invalid option value.")
		}

		if o.namespace == scalingNamespace {
			n, err := strconv.Atoi(*v.Value)
			if err != nil || n < 1 || n > 10000 {
				return invalid("Scaling size must be between 1 and 10000.")
			}
		}

		if len(*v.Value) > 4096 {
			return invalid("Option value too long.")
		}

		return nil
	}

	return invalid("Unsupported configuration option.")
}

func mergeOptions(
	current, add []api.ConfigurationOptionSetting,
	remove []api.OptionSpecification,
) ([]api.ConfigurationOptionSetting, error) {
	out := slices.Clone(current)
	seen := map[string]bool{}
	for _, v := range add {
		if err := checkOption(v); err != nil {
			return nil, err
		}

		key := common.Value(v.Namespace) + "/" + *v.OptionName
		if seen[key] {
			return nil, invalid("Duplicate configuration option.")
		}

		seen[key] = true
		out = slices.DeleteFunc(out, func(x api.ConfigurationOptionSetting) bool {
			return matches(v.Namespace, x.Namespace) && matches(v.OptionName, x.OptionName)
		})

		out = append(out, v)
	}

	for _, v := range remove {
		if v.ResourceName != nil || v.Namespace == nil || v.OptionName == nil {
			return nil, invalid("Namespace and OptionName required; ResourceName unsupported.")
		}

		if err := checkOption(
			api.ConfigurationOptionSetting{Namespace: v.Namespace, OptionName: v.OptionName, Value: new("")},
		); err != nil {
			// Validate the option's identity with its fixed default rather than a removed value.
			known := false
			for _, o := range options {
				known = known || (*v.Namespace == o.namespace && (o.name == "*" || *v.OptionName == o.name))
			}

			if !known {
				return nil, err
			}
		}

		if seen[*v.Namespace+"/"+*v.OptionName] {
			return nil, invalid("Cannot set and remove the same option.")
		}

		out = slices.DeleteFunc(out, func(x api.ConfigurationOptionSetting) bool {
			return matches(v.Namespace, x.Namespace) && matches(v.OptionName, x.OptionName)
		})
	}

	return out, nil
}

func config(
	tx storage.ReadTx,
	sc request.Scope,
	app, template, env *string,
) (*api.ConfigurationSettingsDescription, error) {
	if app == nil {
		return nil, invalid("ApplicationName required.")
	}

	if _, err := read(tx, sc, kindApplication, *app); err != nil {
		return nil, err
	}

	if (template == nil) == (env == nil) {
		return nil, invalid("Specify exactly one TemplateName or EnvironmentName.")
	}

	if template != nil {
		r, e := read(tx, sc, kindTemplate, *app+"/"+*template)
		if e != nil {
			return nil, e
		}

		return r.Template, nil
	}

	r, e := environment(tx, sc, nil, env)
	if e != nil {
		return nil, e
	}

	v := r.Environment
	if *v.ApplicationName != *app {
		return nil, invalid("Environment does not belong to application.")
	}

	return &api.ConfigurationSettingsDescription{
		ApplicationName:   app,
		EnvironmentName:   env,
		SolutionStackName: v.SolutionStackName,
		PlatformArn:       v.PlatformArn,
		Description:       v.Description,
		DateCreated:       v.DateCreated,
		DateUpdated:       v.DateUpdated,
		DeploymentStatus:  new("deployed"),
		OptionSettings:    r.Options,
	}, nil
}

func (s *Service) CreateConfigurationTemplate(
	ctx context.Context,
	in *api.CreateConfigurationTemplateMessage,
) (*api.ConfigurationSettingsDescription, error) {
	if in.Description != nil && len(*in.Description) > 200 {
		return nil, invalid("Description must be at most 200 characters.")
	}

	if err := validName(in.TemplateName); err != nil {
		return nil, err
	}

	tt, err := tags(in.Tags)
	if err != nil {
		return nil, err
	}

	var out *api.ConfigurationSettingsDescription
	err = s.change(ctx, func(tx storage.WriteTx, sc request.Scope) error {
		if _, e := read(tx, sc, kindApplication, common.Value(in.ApplicationName)); e != nil {
			return e
		}

		name := *in.ApplicationName + "/" + *in.TemplateName
		if _, ok, e := storage.Read[record](tx, sc.Key(serviceName, kindTemplate, name)); e != nil {
			return e
		} else if ok {
			return invalid("Configuration template already exists.")
		}

		stack, pa := in.SolutionStackName, in.PlatformArn
		var current []api.ConfigurationOptionSetting
		if in.EnvironmentId != nil {
			if stack != nil || pa != nil {
				return invalid("EnvironmentId and platform cannot both be specified.")
			}

			r, e := environment(tx, sc, in.EnvironmentId, nil)
			if e != nil {
				return e
			}

			if *r.Environment.ApplicationName != *in.ApplicationName {
				return invalid("Environment does not belong to application.")
			}

			stack = r.Environment.SolutionStackName
			current = r.Options
		}

		p, e := resolvePlatform(sc, stack, pa)
		if e != nil {
			return e
		}

		opts, e := mergeOptions(current, in.OptionSettings, nil)
		if e != nil {
			return e
		}

		now := time.Now().UTC()
		out = &api.ConfigurationSettingsDescription{
			ApplicationName:   in.ApplicationName,
			TemplateName:      in.TemplateName,
			Description:       in.Description,
			SolutionStackName: new(p.stack()),
			PlatformArn:       p.arn(sc.Region),
			DateCreated:       new(now),
			DateUpdated:       new(now),
			OptionSettings:    opts,
		}

		return put(tx, sc, kindTemplate, name, &record{Template: out, Tags: tt})
	})

	return out, err
}

func (s *Service) DescribeConfigurationSettings(
	ctx context.Context,
	in *api.DescribeConfigurationSettingsMessage,
) (*api.ConfigurationSettingsDescriptions, error) {
	out := &api.ConfigurationSettingsDescriptions{}
	err := s.view(ctx, func(tx storage.ReadTx, sc request.Scope) error {
		v, e := config(tx, sc, in.ApplicationName, in.TemplateName, in.EnvironmentName)
		if e == nil {
			out.ConfigurationSettings = []api.ConfigurationSettingsDescription{*v}
		}

		return e
	})

	return out, err
}

func (s *Service) UpdateConfigurationTemplate(
	ctx context.Context,
	in *api.UpdateConfigurationTemplateMessage,
) (*api.ConfigurationSettingsDescription, error) {
	if in.Description != nil && len(*in.Description) > 200 {
		return nil, invalid("Description must be at most 200 characters.")
	}

	var out *api.ConfigurationSettingsDescription
	err := s.change(ctx, func(tx storage.WriteTx, sc request.Scope) error {
		name := common.Value(in.ApplicationName) + "/" + common.Value(in.TemplateName)
		r, e := read(tx, sc, kindTemplate, name)
		if e != nil {
			return e
		}

		opts, e := mergeOptions(r.Template.OptionSettings, in.OptionSettings, in.OptionsToRemove)
		if e != nil {
			return e
		}

		r.Template.OptionSettings = opts
		r.Template.DateUpdated = new(time.Now().UTC())
		if in.Description != nil {
			r.Template.Description = in.Description
		}

		out = r.Template
		return put(tx, sc, kindTemplate, name, r)
	})

	return out, err
}

func (s *Service) DeleteConfigurationTemplate(
	ctx context.Context,
	in *api.DeleteConfigurationTemplateMessage,
) (*api.DeleteConfigurationTemplateOutput, error) {
	err := s.change(ctx, func(tx storage.WriteTx, sc request.Scope) error {
		name := common.Value(in.ApplicationName) + "/" + common.Value(in.TemplateName)
		if _, e := read(tx, sc, kindTemplate, name); e != nil {
			return e
		}

		tx.Delete(sc.Key(serviceName, kindTemplate, name))
		return nil
	})

	return &api.DeleteConfigurationTemplateOutput{}, err
}

func (s *Service) DescribeConfigurationOptions(
	ctx context.Context,
	in *api.DescribeConfigurationOptionsMessage,
) (*api.ConfigurationOptionsDescription, error) {
	out := &api.ConfigurationOptionsDescription{}
	err := s.view(ctx, func(tx storage.ReadTx, sc request.Scope) error {
		stack, pa := in.SolutionStackName, in.PlatformArn
		if in.TemplateName != nil || in.EnvironmentName != nil {
			if stack != nil || pa != nil {
				return invalid("Configuration and platform cannot both be specified.")
			}

			c, e := config(tx, sc, in.ApplicationName, in.TemplateName, in.EnvironmentName)
			if e != nil {
				return e
			}

			stack = c.SolutionStackName
		} else if in.ApplicationName != nil {
			if _, e := read(tx, sc, kindApplication, *in.ApplicationName); e != nil {
				return e
			}
		}

		if stack != nil || pa != nil {
			p, e := resolvePlatform(sc, stack, pa)
			if e != nil {
				return e
			}

			out.SolutionStackName = new(p.stack())
			out.PlatformArn = p.arn(sc.Region)
		}

		for _, f := range in.Options {
			if f.ResourceName != nil {
				return invalid("ResourceName is unsupported.")
			}
		}

		for _, o := range options {
			include := len(in.Options) == 0
			for _, f := range in.Options {
				include = include || (matches(f.Namespace, new(o.namespace)) && matches(f.OptionName, new(o.name)))
			}

			if include {
				out.Options = append(
					out.Options,
					api.ConfigurationOptionDescription{
						Namespace:      new(o.namespace),
						Name:           new(o.name),
						DefaultValue:   new(o.def),
						ValueType:      new(o.kind),
						ValueOptions:   o.values,
						UserDefined:    new(o.name == "*"),
						ChangeSeverity: new("NoInterruption"),
					},
				)
			}
		}

		return nil
	})

	return out, err
}

func (s *Service) ValidateConfigurationSettings(
	ctx context.Context,
	in *api.ValidateConfigurationSettingsMessage,
) (*api.ConfigurationSettingsValidationMessages, error) {
	out := &api.ConfigurationSettingsValidationMessages{Messages: []api.ValidationMessage{}}
	err := s.view(ctx, func(tx storage.ReadTx, sc request.Scope) error {
		if _, e := config(tx, sc, in.ApplicationName, in.TemplateName, in.EnvironmentName); e != nil {
			return e
		}

		for _, v := range in.OptionSettings {
			if e := checkOption(v); e != nil {
				out.Messages = append(
					out.Messages,
					api.ValidationMessage{
						Namespace:  v.Namespace,
						OptionName: v.OptionName,
						Severity:   new("error"),
						Message:    new(e.Error()),
					},
				)
			}
		}

		return nil
	})

	return out, err
}

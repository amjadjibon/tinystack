package elasticbeanstalk

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/elasticbeanstalk"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) transition(r *record, status string) {
	e := r.Environment
	e.Status = new(status)
	e.DateUpdated = new(s.now().UTC())
	e.Health = new("Grey")
	e.HealthStatus = new("Pending")
	if status == terminated {
		e.HealthStatus = new("NoData")
	}

	if status == ready {
		e.Health = new("Green")
		e.HealthStatus = new("Ok")
	}

	r.Events = append(
		r.Events,
		api.EventDescription{
			EventDate:       e.DateUpdated,
			ApplicationName: e.ApplicationName,
			EnvironmentName: e.EnvironmentName,
			VersionLabel:    e.VersionLabel,
			TemplateName:    e.TemplateName,
			PlatformArn:     e.PlatformArn,
			Severity:        new("INFO"),
			Message:         new("Environment " + status + ". Records only; no application is deployed."),
		},
	)
}

func saveEnvironment(tx storage.WriteTx, sc request.Scope, r *record) error {
	id := *r.Environment.EnvironmentId
	for _, detail := range r.Events {
		key := fmt.Sprintf("%020d/%s/%s", math.MaxInt64-detail.EventDate.UnixNano(), id, common.RandomID(8))
		ev := eventRecord{Key: key, EnvironmentID: id, Detail: detail}
		if err := storage.Write(tx, sc.Key(serviceName, "event", key), new(ev)); err != nil {
			return err
		}
	}

	r.Events = nil
	index := "aws/elasticbeanstalk/index/" + sc.AccountID + "/" + sc.Region + "/" + id
	if *r.Environment.Status == launching || *r.Environment.Status == updating ||
		*r.Environment.Status == terminating || *r.Environment.Status == terminated {
		if err := storage.Write(tx, index, sc); err != nil {
			return err
		}
	} else {
		tx.Delete(index)
	}

	return put(tx, sc, kindEnvironment, id, r)
}

func (s *Service) tick(ctx context.Context) {
	if err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		now := s.now().UTC()
		for _, k := range tx.Keys("aws/elasticbeanstalk/index/") {
			sc, ok, err := storage.Read[request.Scope](tx, k)
			if err != nil {
				return err
			}

			if !ok {
				tx.Delete(k)
				continue
			}

			id := k[strings.LastIndex(k, "/")+1:]
			r, ok, err := storage.Read[record](tx, sc.Key(serviceName, kindEnvironment, id))
			if err != nil {
				return err
			}

			if !ok {
				tx.Delete(k)
				continue
			}

			if *r.Environment.Status == terminated {
				if r.Environment.DateUpdated != nil && now.Sub(*r.Environment.DateUpdated) > time.Hour {
					tx.Delete(k)
					tx.Delete(sc.Key(serviceName, kindEnvironment, id))
					for _, ek := range tx.Keys(sc.Prefix(serviceName, "event")) {
						ev, evOK, evErr := storage.Read[eventRecord](tx, ek)
						if evErr != nil {
							return evErr
						}

						if evOK && ev.EnvironmentID == id {
							tx.Delete(ek)
						}
					}
				}

				continue
			}

			status := ready
			if *r.Environment.Status == terminating {
				status = terminated
			}

			s.transition(r, status)
			if err := saveEnvironment(tx, *sc, r); err != nil {
				return err
			}
		}

		return nil
	}); err != nil && ctx.Err() == nil {
		s.logger.Warn("update Elastic Beanstalk environments", "error", err)
	}
}

func environment(tx storage.ReadTx, sc request.Scope, id, name *string) (*record, error) {
	if id == nil && name == nil {
		return nil, awserror.New("MissingRequiredParameter", "EnvironmentId or EnvironmentName required.")
	}

	var deleted *record
	for _, k := range tx.Keys(sc.Prefix(serviceName, kindEnvironment)) {
		r, _, err := storage.Read[record](tx, k)
		if err != nil {
			return nil, err
		}

		if matches(id, r.Environment.EnvironmentId) && matches(name, r.Environment.EnvironmentName) {
			if *r.Environment.Status != terminated || id != nil {
				return r, nil
			}

			deleted = r
		}
	}

	if deleted != nil {
		return deleted, nil
	}

	return nil, invalid("No such environment.")
}

func mutable(r *record) error {
	if *r.Environment.Status != ready {
		return invalid("Environment must be Ready.")
	}

	return nil
}

func tier(in *api.EnvironmentTier) (*api.EnvironmentTier, error) {
	if in == nil {
		return &api.EnvironmentTier{Name: new(webServer), Type: new("Standard"), Version: new("1.0")}, nil
	}

	t := common.Clone(in)
	name := common.Value(t.Name)
	typ := "Standard"
	if name == "Worker" {
		typ = "SQS/HTTP"
	} else if name != webServer {
		return nil, invalid("Tier must be WebServer or Worker.")
	}

	if t.Type != nil && *t.Type != typ {
		return nil, invalid("Invalid tier type.")
	}

	if t.Version != nil && *t.Version != "1.0" && *t.Version != "" {
		return nil, invalid("Invalid tier version.")
	}

	t.Type = new(typ)
	t.Version = new("1.0")
	return t, nil
}

func cname(prefix, region string) *string {
	return new(strings.ToLower(prefix) + "." + region + ".elasticbeanstalk.com")
}

func dnsFree(tx storage.ReadTx, region, name string) (bool, error) {
	for _, k := range tx.Keys("") {
		_, rr, rest, ok := storage.Scoped(k, serviceName)
		if !ok || rr != region || !strings.HasPrefix(rest, "environment/") {
			continue
		}

		r, _, err := storage.Read[record](tx, k)
		if err != nil {
			return false, err
		}

		if common.Value(r.Environment.CNAME) == name && *r.Environment.Status != terminated {
			return false, nil
		}
	}

	return true, nil
}

func (s *Service) CreateEnvironment(
	ctx context.Context,
	in *api.CreateEnvironmentMessage,
) (*api.EnvironmentDescription, error) {
	if in.Description != nil && len(*in.Description) > 200 {
		return nil, invalid("Description must be at most 200 characters.")
	}

	if !envPattern.MatchString(common.Value(in.EnvironmentName)) {
		return nil, invalid(
			"EnvironmentName must be 4 to 40 alphanumeric or hyphen characters, starting with a letter.",
		)
	}

	if err := validName(in.ApplicationName); err != nil {
		return nil, err
	}

	tt, err := tags(in.Tags)
	if err != nil {
		return nil, err
	}

	tr, err := tier(in.Tier)
	if err != nil {
		return nil, err
	}

	prefix := *common.OrElse(in.CNAMEPrefix, new(*in.EnvironmentName+"-"+common.RandomID(6)))
	if !cnamePattern.MatchString(prefix) {
		return nil, invalid("Invalid CNAMEPrefix.")
	}

	var out *api.EnvironmentDescription
	err = s.change(ctx, func(tx storage.WriteTx, sc request.Scope) error {
		if _, e := read(tx, sc, kindApplication, *in.ApplicationName); e != nil {
			return e
		}

		for _, k := range tx.Keys(sc.Prefix(serviceName, kindEnvironment)) {
			r, _, e := storage.Read[record](tx, k)
			if e != nil {
				return e
			}

			if *r.Environment.EnvironmentName == *in.EnvironmentName && *r.Environment.Status != terminated {
				return invalid("Environment already exists.")
			}
		}

		free, e := dnsFree(tx, sc.Region, *cname(prefix, sc.Region))
		if e != nil {
			return e
		}

		if !free {
			return invalid("CNAME is already in use.")
		}

		if in.SolutionStackName != nil && in.PlatformArn != nil {
			return invalid("Specify SolutionStackName or PlatformArn, not both.")
		}

		cfg := &api.ConfigurationSettingsDescription{
			SolutionStackName: in.SolutionStackName,
			PlatformArn:       in.PlatformArn,
		}

		if in.TemplateName != nil {
			if in.SolutionStackName != nil || in.PlatformArn != nil {
				return invalid("Template and platform cannot both be specified.")
			}

			r, templateErr := read(tx, sc, kindTemplate, *in.ApplicationName+"/"+*in.TemplateName)
			if templateErr != nil {
				return templateErr
			}

			cfg = r.Template
		}

		p, e := resolvePlatform(sc, cfg.SolutionStackName, nil)
		if cfg.SolutionStackName == nil {
			p, e = resolvePlatform(sc, nil, cfg.PlatformArn)
		}

		if e != nil {
			return e
		}

		opts, e := mergeOptions(cfg.OptionSettings, in.OptionSettings, nil)
		if e != nil {
			return e
		}

		if in.VersionLabel != nil {
			if _, e = read(tx, sc, kindVersion, *in.ApplicationName+"/"+*in.VersionLabel); e != nil {
				return e
			}
		}

		id := "e-" + common.RandomID(10)
		now := s.now().UTC()
		out = &api.EnvironmentDescription{
			EnvironmentName:   in.EnvironmentName,
			EnvironmentId:     new(id),
			ApplicationName:   in.ApplicationName,
			VersionLabel:      in.VersionLabel,
			Description:       in.Description,
			TemplateName:      in.TemplateName,
			SolutionStackName: new(p.stack()),
			PlatformArn:       p.arn(sc.Region),
			CNAME:             cname(prefix, sc.Region),
			EndpointURL:       cname(prefix, sc.Region),
			DateCreated:       new(now),
			Tier:              tr,
			EnvironmentArn:    arn(sc, kindEnvironment, *in.ApplicationName+"/"+*in.EnvironmentName),
		}

		r := &record{Environment: out, Options: opts, Tags: tt}
		s.transition(r, launching)
		return saveEnvironment(tx, sc, r)
	})

	return out, err
}

func (s *Service) DescribeEnvironments(
	ctx context.Context,
	in *api.DescribeEnvironmentsMessage,
) (*api.EnvironmentDescriptionsMessage, error) {
	n, err := limit(in.MaxRecords)
	if err != nil {
		return nil, err
	}

	includeDeleted := *common.OrElse(in.IncludeDeleted, new(true))
	if in.IncludedDeletedBackTo != nil && !includeDeleted {
		return nil, invalid("IncludeDeleted required with IncludedDeletedBackTo.")
	}

	out := &api.EnvironmentDescriptionsMessage{}
	err = s.view(ctx, func(tx storage.ReadTx, sc request.Scope) error {
		for _, k := range tx.Keys(sc.Prefix(serviceName, kindEnvironment)) {
			r, _, e := storage.Read[record](tx, k)
			if e != nil {
				return e
			}

			v := r.Environment
			if !matches(in.ApplicationName, v.ApplicationName) || !matches(in.VersionLabel, v.VersionLabel) ||
				!contains(in.EnvironmentIds, v.EnvironmentId) ||
				!contains(in.EnvironmentNames, v.EnvironmentName) {
				continue
			}

			tooOld := in.IncludedDeletedBackTo != nil &&
				(v.DateUpdated == nil || v.DateUpdated.Before(*in.IncludedDeletedBackTo))
			if *v.Status == terminated && (!includeDeleted || tooOld) {
				continue
			}

			out.Environments = append(out.Environments, *v)
		}

		var e error
		out.Environments, out.NextToken, e = common.Page(
			out.Environments,
			func(v api.EnvironmentDescription) string { return *v.EnvironmentId },
			in.NextToken,
			n,
			invalid("Invalid NextToken."),
		)
		return e
	})

	return out, err
}

func (s *Service) changeEnvironment(
	ctx context.Context,
	id, name *string,
	fn func(storage.WriteTx, request.Scope, *record) error,
) (*api.EnvironmentDescription, error) {
	var out *api.EnvironmentDescription
	err := s.change(ctx, func(tx storage.WriteTx, sc request.Scope) error {
		r, e := environment(tx, sc, id, name)
		if e != nil {
			return e
		}

		if e := fn(tx, sc, r); e != nil {
			return e
		}

		out = r.Environment
		return saveEnvironment(tx, sc, r)
	})

	return out, err
}

func (s *Service) UpdateEnvironment(
	ctx context.Context,
	in *api.UpdateEnvironmentMessage,
) (*api.EnvironmentDescription, error) {
	if in.Description != nil && len(*in.Description) > 200 {
		return nil, invalid("Description must be at most 200 characters.")
	}

	return s.changeEnvironment(
		ctx,
		in.EnvironmentId,
		in.EnvironmentName,
		func(tx storage.WriteTx, sc request.Scope, r *record) error {
			if e := mutable(r); e != nil {
				return e
			}

			v := r.Environment
			if !matches(in.ApplicationName, v.ApplicationName) {
				return invalid("ApplicationName does not match environment.")
			}

			if in.Tier != nil {
				t, e := tier(in.Tier)
				if e != nil {
					return e
				}

				if *t.Name != *v.Tier.Name {
					return invalid("Cannot change environment tier.")
				}
			}

			if in.TemplateName != nil && (in.PlatformArn != nil || in.SolutionStackName != nil) {
				return invalid("Template and platform cannot both be specified.")
			}

			if in.TemplateName != nil {
				t, e := read(tx, sc, kindTemplate, *v.ApplicationName+"/"+*in.TemplateName)
				if e != nil {
					return e
				}

				v.TemplateName = in.TemplateName
				v.PlatformArn = t.Template.PlatformArn
				v.SolutionStackName = t.Template.SolutionStackName
				r.Options = t.Template.OptionSettings
			}

			if in.SolutionStackName != nil || in.PlatformArn != nil {
				p, e := resolvePlatform(sc, in.SolutionStackName, in.PlatformArn)
				if e != nil {
					return e
				}

				v.PlatformArn = p.arn(sc.Region)
				v.SolutionStackName = new(p.stack())
			}

			if in.VersionLabel != nil {
				if _, e := read(tx, sc, kindVersion, *v.ApplicationName+"/"+*in.VersionLabel); e != nil {
					return e
				}

				v.VersionLabel = in.VersionLabel
			}

			opts, e := mergeOptions(r.Options, in.OptionSettings, in.OptionsToRemove)
			if e != nil {
				return e
			}

			r.Options = opts
			if in.Description != nil {
				v.Description = in.Description
			}

			s.transition(r, updating)
			return nil
		},
	)
}

func (s *Service) TerminateEnvironment(
	ctx context.Context,
	in *api.TerminateEnvironmentMessage,
) (*api.EnvironmentDescription, error) {
	return s.changeEnvironment(
		ctx,
		in.EnvironmentId,
		in.EnvironmentName,
		func(_ storage.WriteTx, _ request.Scope, r *record) error {
			if *r.Environment.Status == terminated || *r.Environment.Status == terminating {
				return nil
			}

			r.RetainResources = in.TerminateResources != nil && !*in.TerminateResources
			s.transition(r, terminating)
			return nil
		},
	)
}

func (s *Service) RebuildEnvironment(
	ctx context.Context,
	in *api.RebuildEnvironmentMessage,
) (*api.RebuildEnvironmentOutput, error) {
	_, err := s.changeEnvironment(
		ctx,
		in.EnvironmentId,
		in.EnvironmentName,
		func(_ storage.WriteTx, _ request.Scope, r *record) error {
			if e := mutable(r); e != nil {
				return e
			}

			s.transition(r, launching)
			return nil
		},
	)
	return &api.RebuildEnvironmentOutput{}, err
}

func (s *Service) RestartAppServer(
	ctx context.Context,
	in *api.RestartAppServerMessage,
) (*api.RestartAppServerOutput, error) {
	_, err := s.changeEnvironment(
		ctx,
		in.EnvironmentId,
		in.EnvironmentName,
		func(_ storage.WriteTx, _ request.Scope, r *record) error {
			if e := mutable(r); e != nil {
				return e
			}

			s.transition(r, updating)
			return nil
		},
	)
	return &api.RestartAppServerOutput{}, err
}

func (s *Service) SwapEnvironmentCNAMEs(
	ctx context.Context,
	in *api.SwapEnvironmentCNAMEsMessage,
) (*api.SwapEnvironmentCNAMEsOutput, error) {
	err := s.change(ctx, func(tx storage.WriteTx, sc request.Scope) error {
		a, e := environment(tx, sc, in.SourceEnvironmentId, in.SourceEnvironmentName)
		if e != nil {
			return e
		}

		b, e := environment(tx, sc, in.DestinationEnvironmentId, in.DestinationEnvironmentName)
		if e != nil {
			return e
		}

		if *a.Environment.EnvironmentId == *b.Environment.EnvironmentId {
			return invalid("Source and destination must differ.")
		}

		if e := mutable(a); e != nil {
			return e
		}

		if e := mutable(b); e != nil {
			return e
		}

		if *a.Environment.Tier.Name != webServer || *b.Environment.Tier.Name != webServer {
			return invalid("CNAME swaps require WebServer environments.")
		}

		a.Environment.CNAME, b.Environment.CNAME = b.Environment.CNAME, a.Environment.CNAME
		a.Environment.EndpointURL, b.Environment.EndpointURL = a.Environment.CNAME, b.Environment.CNAME
		s.transition(a, updating)
		s.transition(b, updating)
		if e := saveEnvironment(tx, sc, a); e != nil {
			return e
		}

		return saveEnvironment(tx, sc, b)
	})

	return &api.SwapEnvironmentCNAMEsOutput{}, err
}

func (s *Service) CheckDNSAvailability(
	ctx context.Context,
	in *api.CheckDNSAvailabilityMessage,
) (*api.CheckDNSAvailabilityResultMessage, error) {
	if !cnamePattern.MatchString(common.Value(in.CNAMEPrefix)) {
		return nil, invalid("Invalid CNAMEPrefix.")
	}

	out := &api.CheckDNSAvailabilityResultMessage{}
	err := s.view(ctx, func(tx storage.ReadTx, sc request.Scope) error {
		out.FullyQualifiedCNAME = cname(*in.CNAMEPrefix, sc.Region)
		free, e := dnsFree(tx, sc.Region, *out.FullyQualifiedCNAME)
		out.Available = new(free)
		return e
	})

	return out, err
}

func (s *Service) DescribeEnvironmentResources(
	ctx context.Context,
	in *api.DescribeEnvironmentResourcesMessage,
) (*api.EnvironmentResourceDescriptionsMessage, error) {
	out := &api.EnvironmentResourceDescriptionsMessage{}
	err := s.view(ctx, func(tx storage.ReadTx, sc request.Scope) error {
		r, e := environment(tx, sc, in.EnvironmentId, in.EnvironmentName)
		if e != nil {
			return e
		}

		v := r.Environment
		res := &api.EnvironmentResourceDescription{EnvironmentName: v.EnvironmentName}
		if *v.Status != terminated || r.RetainResources {
			res.AutoScalingGroups = []api.AutoScalingGroup{
				{Name: new("awseb-" + *v.EnvironmentId + "-stack-AWSEBAutoScalingGroup")},
			}

			res.Instances = []api.Instance{{Id: new("i-" + strings.TrimPrefix(*v.EnvironmentId, "e-"))}}
			if *v.Tier.Name == webServer {
				res.LoadBalancers = []api.LoadBalancer{{Name: new("awseb-" + *v.EnvironmentId)}}
			} else {
				res.Queues = []api.Queue{
					{
						Name: new("awseb-" + *v.EnvironmentId + "-AWSEBWorkerQueue"),
						URL: new(
							"https://sqs." + sc.Region + ".amazonaws.com/" + sc.AccountID + "/awseb-" + *v.EnvironmentId,
						),
					},
				}
			}
		}

		out.EnvironmentResources = res
		return nil
	})

	return out, err
}

func (s *Service) DescribeEnvironmentHealth(
	ctx context.Context,
	in *api.DescribeEnvironmentHealthRequest,
) (*api.DescribeEnvironmentHealthResult, error) {
	out := &api.DescribeEnvironmentHealthResult{}
	err := s.view(ctx, func(tx storage.ReadTx, sc request.Scope) error {
		r, e := environment(tx, sc, in.EnvironmentId, in.EnvironmentName)
		if e != nil {
			return e
		}

		v := r.Environment
		out.EnvironmentName = v.EnvironmentName
		out.Status = v.Status
		out.Color = v.Health
		out.HealthStatus = v.HealthStatus
		out.RefreshedAt = new(s.now().UTC())
		out.Causes = []string{"Synthetic health; records only, no application is running."}
		return nil
	})

	return out, err
}

func (s *Service) DescribeEvents(
	ctx context.Context,
	in *api.DescribeEventsMessage,
) (*api.EventDescriptionsMessage, error) {
	n, err := limit(in.MaxRecords)
	if err != nil {
		return nil, err
	}

	if in.StartTime != nil && in.EndTime != nil && in.StartTime.After(*in.EndTime) {
		return nil, invalid("StartTime must precede EndTime.")
	}

	out := &api.EventDescriptionsMessage{}
	err = s.view(ctx, func(tx storage.ReadTx, sc request.Scope) error {
		selected := []eventRecord{}
		for _, k := range tx.Keys(sc.Prefix(serviceName, "event")) {
			ev, _, e := storage.Read[eventRecord](tx, k)
			if e != nil {
				return e
			}

			if in.EnvironmentId != nil && *in.EnvironmentId != ev.EnvironmentID {
				continue
			}

			v := ev.Detail
			if !matches(in.ApplicationName, v.ApplicationName) || !matches(in.EnvironmentName, v.EnvironmentName) ||
				!matches(in.VersionLabel, v.VersionLabel) || !matches(in.TemplateName, v.TemplateName) ||
				!matches(in.PlatformArn, v.PlatformArn) || !matches(in.Severity, v.Severity) {
				continue
			}

			if in.StartTime != nil && v.EventDate.Before(*in.StartTime) ||
				in.EndTime != nil && v.EventDate.After(*in.EndTime) {
				continue
			}

			selected = append(selected, *ev)
		}

		page, next, e := common.Page(
			selected,
			func(v eventRecord) string { return v.Key },
			in.NextToken,
			n,
			invalid("Invalid NextToken."),
		)
		if e != nil {
			return e
		}

		out.NextToken = next
		for _, v := range page {
			out.Events = append(out.Events, v.Detail)
		}

		return nil
	})

	return out, err
}

type eventRecord struct {
	Detail        api.EventDescription
	Key           string
	EnvironmentID string
}

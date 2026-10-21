package elasticbeanstalk

import (
	"context"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/elasticbeanstalk"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func lifecycle(c *api.ApplicationResourceLifecycleConfig) error {
	if c == nil {
		return nil
	}

	if c.VersionLifecycleConfig != nil {
		v := c.VersionLifecycleConfig
		if v.MaxCountRule != nil && common.Value(v.MaxCountRule.Enabled) && common.Value(v.MaxCountRule.MaxCount) < 1 {
			return invalid("MaxCount must be positive.")
		}

		if v.MaxAgeRule != nil && common.Value(v.MaxAgeRule.Enabled) && common.Value(v.MaxAgeRule.MaxAgeInDays) < 1 {
			return invalid("MaxAgeInDays must be positive.")
		}
	}

	return nil
}

func createApp(tx storage.WriteTx, sc request.Scope, in *api.CreateApplicationMessage) (*record, error) {
	if err := validName(in.ApplicationName); err != nil {
		return nil, err
	}

	if err := lifecycle(in.ResourceLifecycleConfig); err != nil {
		return nil, err
	}

	tt, err := tags(in.Tags)
	if err != nil {
		return nil, err
	}

	key := sc.Key(serviceName, kindApplication, *in.ApplicationName)
	if _, ok, e := storage.Read[record](tx, key); e != nil {
		return nil, e
	} else if ok {
		return nil, invalid("Application already exists.")
	}

	now := time.Now().UTC()
	r := &record{
		Application: &api.ApplicationDescription{
			ApplicationName:         in.ApplicationName,
			ApplicationArn:          arn(sc, kindApplication, *in.ApplicationName),
			Description:             in.Description,
			DateCreated:             new(now),
			DateUpdated:             new(now),
			ResourceLifecycleConfig: in.ResourceLifecycleConfig,
		},
		Tags: tt,
	}

	return r, storage.Write(tx, key, r)
}

func (s *Service) CreateApplication(
	ctx context.Context,
	in *api.CreateApplicationMessage,
) (*api.ApplicationDescriptionMessage, error) {
	if in.Description != nil && len(*in.Description) > 200 {
		return nil, invalid("Description must be at most 200 characters.")
	}

	out := &api.ApplicationDescriptionMessage{}
	err := s.change(ctx, func(tx storage.WriteTx, sc request.Scope) error {
		r, e := createApp(tx, sc, in)
		if e == nil {
			out.Application = r.Application
		}

		return e
	})

	return out, err
}

func (s *Service) DescribeApplications(
	ctx context.Context,
	in *api.DescribeApplicationsMessage,
) (*api.ApplicationDescriptionsMessage, error) {
	out := &api.ApplicationDescriptionsMessage{Applications: []api.ApplicationDescription{}}
	err := s.view(ctx, func(tx storage.ReadTx, sc request.Scope) error {
		for _, k := range tx.Keys(sc.Prefix(serviceName, kindApplication)) {
			r, _, e := storage.Read[record](tx, k)
			if e != nil {
				return e
			}

			if !contains(in.ApplicationNames, r.Application.ApplicationName) {
				continue
			}

			a := r.Application
			for _, vk := range tx.Keys(sc.Prefix(serviceName, kindVersion) + *a.ApplicationName + "/") {
				v, _, e := storage.Read[record](tx, vk)
				if e != nil {
					return e
				}

				a.Versions = append(a.Versions, *v.Version.VersionLabel)
			}

			for _, tk := range tx.Keys(sc.Prefix(serviceName, kindTemplate) + *a.ApplicationName + "/") {
				v, _, e := storage.Read[record](tx, tk)
				if e != nil {
					return e
				}

				a.ConfigurationTemplates = append(a.ConfigurationTemplates, *v.Template.TemplateName)
			}

			out.Applications = append(out.Applications, *a)
		}

		return nil
	})

	return out, err
}

func (s *Service) UpdateApplication(
	ctx context.Context,
	in *api.UpdateApplicationMessage,
) (*api.ApplicationDescriptionMessage, error) {
	if in.Description != nil && len(*in.Description) > 200 {
		return nil, invalid("Description must be at most 200 characters.")
	}

	out := &api.ApplicationDescriptionMessage{}
	err := s.change(ctx, func(tx storage.WriteTx, sc request.Scope) error {
		r, e := read(tx, sc, kindApplication, common.Value(in.ApplicationName))
		if e != nil {
			return e
		}

		r.Application.Description = in.Description
		r.Application.DateUpdated = new(time.Now().UTC())
		out.Application = r.Application
		return put(tx, sc, kindApplication, *in.ApplicationName, r)
	})

	return out, err
}

func (s *Service) UpdateApplicationResourceLifecycle(
	ctx context.Context,
	in *api.UpdateApplicationResourceLifecycleMessage,
) (*api.ApplicationResourceLifecycleDescriptionMessage, error) {
	if in.ResourceLifecycleConfig == nil {
		return nil, invalid("ResourceLifecycleConfig required.")
	}

	if err := lifecycle(in.ResourceLifecycleConfig); err != nil {
		return nil, err
	}

	err := s.change(ctx, func(tx storage.WriteTx, sc request.Scope) error {
		r, e := read(tx, sc, kindApplication, common.Value(in.ApplicationName))
		if e != nil {
			return e
		}

		r.Application.ResourceLifecycleConfig = in.ResourceLifecycleConfig
		r.Application.DateUpdated = new(time.Now().UTC())
		return put(tx, sc, kindApplication, *in.ApplicationName, r)
	})

	return &api.ApplicationResourceLifecycleDescriptionMessage{
		ApplicationName:         in.ApplicationName,
		ResourceLifecycleConfig: in.ResourceLifecycleConfig,
	}, err
}

func (s *Service) DeleteApplication(
	ctx context.Context,
	in *api.DeleteApplicationMessage,
) (*api.DeleteApplicationOutput, error) {
	err := s.change(ctx, func(tx storage.WriteTx, sc request.Scope) error {
		if _, e := read(tx, sc, kindApplication, common.Value(in.ApplicationName)); e != nil {
			return e
		}

		for _, k := range tx.Keys(sc.Prefix(serviceName, kindEnvironment)) {
			r, _, e := storage.Read[record](tx, k)
			if e != nil {
				return e
			}

			if !matches(in.ApplicationName, r.Environment.ApplicationName) || *r.Environment.Status == terminated {
				continue
			}

			if !common.Value(in.TerminateEnvByForce) {
				return invalid("Application has active environments.")
			}

			transition(r, terminating)
			if e := saveEnvironment(tx, sc, r); e != nil {
				return e
			}
		}

		for _, kind := range []string{kindVersion, kindTemplate} {
			for _, k := range tx.Keys(sc.Prefix(serviceName, kind) + *in.ApplicationName + "/") {
				tx.Delete(k)
			}
		}

		tx.Delete(sc.Key(serviceName, kindApplication, *in.ApplicationName))
		return nil
	})

	return &api.DeleteApplicationOutput{}, err
}

func (s *Service) CreateApplicationVersion(
	ctx context.Context,
	in *api.CreateApplicationVersionMessage,
) (*api.ApplicationVersionDescriptionMessage, error) {
	if in.Description != nil && len(*in.Description) > 200 {
		return nil, invalid("Description must be at most 200 characters.")
	}

	if err := validName(in.ApplicationName); err != nil {
		return nil, err
	}

	if err := validName(in.VersionLabel); err != nil {
		return nil, err
	}

	tt, err := tags(in.Tags)
	if err != nil {
		return nil, err
	}

	if in.SourceBundle != nil {
		b := in.SourceBundle
		if common.Value(b.S3Bucket) == "" || common.Value(b.S3Key) == "" ||
			len(common.Value(b.S3Bucket)) > 255 || len(common.Value(b.S3Key)) > 1024 {
			return nil, invalid("SourceBundle requires S3Bucket and S3Key.")
		}

		if common.Value(in.Process) {
			_, bucketErr := s.buckets.HeadBucket(ctx, &s3api.HeadBucketRequest{Bucket: b.S3Bucket})
			if bucketErr != nil {
				if awserror.From(bucketErr).Code == "PermanentRedirect" {
					return nil, awserror.New(
						"S3LocationNotInServiceRegionException",
						"Source bucket must be in the same Region.",
					)
				}

				return nil, invalid("Unable to access source bucket in S3.")
			}

			if _, err = s.buckets.HeadObject(
				ctx,
				&s3api.HeadObjectRequest{Bucket: b.S3Bucket, Key: b.S3Key},
			); err != nil {
				return nil, invalid("Unable to access source bundle in S3.")
			}
		}
	}

	out := &api.ApplicationVersionDescriptionMessage{}
	err = s.change(ctx, func(tx storage.WriteTx, sc request.Scope) error {
		if _, e := read(tx, sc, kindApplication, *in.ApplicationName); e != nil {
			if !common.Value(in.AutoCreateApplication) {
				return e
			}

			if _, e = createApp(tx, sc, &api.CreateApplicationMessage{ApplicationName: in.ApplicationName}); e != nil {
				return e
			}
		}

		name := *in.ApplicationName + "/" + *in.VersionLabel
		if _, ok, e := storage.Read[record](tx, sc.Key(serviceName, kindVersion, name)); e != nil {
			return e
		} else if ok {
			return invalid("Application version already exists.")
		}

		now := time.Now().UTC()
		status := "Unprocessed"
		if common.Value(in.Process) {
			status = "Processed"
		}

		v := &api.ApplicationVersionDescription{
			ApplicationName:       in.ApplicationName,
			VersionLabel:          in.VersionLabel,
			Description:           in.Description,
			SourceBundle:          in.SourceBundle,
			Process:               in.Process,
			Status:                new(status),
			DateCreated:           new(now),
			DateUpdated:           new(now),
			ApplicationVersionArn: arn(sc, kindApplicationVersion, name),
		}

		out.ApplicationVersion = v
		return put(tx, sc, kindVersion, name, &record{Version: v, Tags: tt})
	})

	return out, err
}

func (s *Service) DescribeApplicationVersions(
	ctx context.Context,
	in *api.DescribeApplicationVersionsMessage,
) (*api.ApplicationVersionDescriptionsMessage, error) {
	n, err := limit(in.MaxRecords)
	if err != nil {
		return nil, err
	}

	out := &api.ApplicationVersionDescriptionsMessage{}
	err = s.view(ctx, func(tx storage.ReadTx, sc request.Scope) error {
		for _, k := range tx.Keys(sc.Prefix(serviceName, kindVersion)) {
			r, _, e := storage.Read[record](tx, k)
			if e != nil {
				return e
			}

			if matches(in.ApplicationName, r.Version.ApplicationName) &&
				contains(in.VersionLabels, r.Version.VersionLabel) {
				out.ApplicationVersions = append(out.ApplicationVersions, *r.Version)
			}
		}

		var e error
		out.ApplicationVersions, out.NextToken, e = common.Page(
			out.ApplicationVersions,
			func(v api.ApplicationVersionDescription) string { return *v.ApplicationName + "/" + *v.VersionLabel },
			in.NextToken,
			n,
			invalid("Invalid NextToken."),
		)
		return e
	})

	return out, err
}

func (s *Service) UpdateApplicationVersion(
	ctx context.Context,
	in *api.UpdateApplicationVersionMessage,
) (*api.ApplicationVersionDescriptionMessage, error) {
	if in.Description != nil && len(*in.Description) > 200 {
		return nil, invalid("Description must be at most 200 characters.")
	}

	out := &api.ApplicationVersionDescriptionMessage{}
	err := s.change(ctx, func(tx storage.WriteTx, sc request.Scope) error {
		name := common.Value(in.ApplicationName) + "/" + common.Value(in.VersionLabel)
		r, e := read(tx, sc, kindVersion, name)
		if e != nil {
			return e
		}

		r.Version.Description = in.Description
		r.Version.DateUpdated = new(time.Now().UTC())
		out.ApplicationVersion = r.Version
		return put(tx, sc, kindVersion, name, r)
	})

	return out, err
}

func (s *Service) DeleteApplicationVersion(
	ctx context.Context,
	in *api.DeleteApplicationVersionMessage,
) (*api.DeleteApplicationVersionOutput, error) {
	var bundle *api.S3Location
	err := s.change(ctx, func(tx storage.WriteTx, sc request.Scope) error {
		name := common.Value(in.ApplicationName) + "/" + common.Value(in.VersionLabel)
		r, e := read(tx, sc, kindVersion, name)
		if e != nil {
			return e
		}

		for _, k := range tx.Keys(sc.Prefix(serviceName, kindEnvironment)) {
			env, _, e := storage.Read[record](tx, k)
			if e != nil {
				return e
			}

			if matches(in.ApplicationName, env.Environment.ApplicationName) &&
				matches(in.VersionLabel, env.Environment.VersionLabel) &&
				*env.Environment.Status != terminated {
				return invalid("Application version is in use.")
			}
		}

		bundle = r.Version.SourceBundle
		tx.Delete(sc.Key(serviceName, kindVersion, name))
		return nil
	})

	if err == nil && common.Value(in.DeleteSourceBundle) && bundle != nil {
		_, err = s.buckets.DeleteObject(ctx, &s3api.DeleteObjectRequest{Bucket: bundle.S3Bucket, Key: bundle.S3Key})
	}

	return &api.DeleteApplicationVersionOutput{}, err
}

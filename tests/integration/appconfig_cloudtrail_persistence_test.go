package integration_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/appconfig"
	"github.com/aws/aws-sdk-go-v2/service/appconfigdata"
	"github.com/aws/aws-sdk-go-v2/service/cloudtrail"
	"github.com/aws/aws-sdk-go-v2/service/cloudtrail/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestAppConfigCloudTrailPersistentRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	c := appconfigClient(s, "test", "us-east-1")
	app, err := c.CreateApplication(ctx, &appconfig.CreateApplicationInput{Name: new("persistent-app")})
	if err != nil {
		stop()
		t.Fatal(err)
	}

	env, err := c.CreateEnvironment(
		ctx,
		&appconfig.CreateEnvironmentInput{ApplicationId: app.Id, Name: new("persistent-env")},
	)
	if err != nil {
		stop()
		t.Fatal(err)
	}

	profile, err := c.CreateConfigurationProfile(
		ctx,
		&appconfig.CreateConfigurationProfileInput{
			ApplicationId: app.Id,
			Name:          new("persistent-config"),
			LocationUri:   new("hosted"),
		},
	)
	if err != nil {
		stop()
		t.Fatal(err)
	}

	_, err = c.CreateHostedConfigurationVersion(
		ctx,
		&appconfig.CreateHostedConfigurationVersionInput{
			ApplicationId:          app.Id,
			ConfigurationProfileId: profile.Id,
			Content:                []byte("persistent-content"),
			ContentType:            new("text/plain"),
		},
	)
	if err != nil {
		stop()
		t.Fatal(err)
	}

	_, err = c.StartDeployment(
		ctx,
		&appconfig.StartDeploymentInput{
			ApplicationId:          app.Id,
			EnvironmentId:          env.Id,
			ConfigurationProfileId: profile.Id,
			ConfigurationVersion:   new("1"),
			DeploymentStrategyId:   new("AppConfig.AllAtOnce"),
		},
	)
	if err != nil {
		stop()
		t.Fatal(err)
	}

	session, err := appconfigDataClient(
		s,
		"test",
		"us-east-1",
	).StartConfigurationSession(ctx,
		&appconfigdata.StartConfigurationSessionInput{
			ApplicationIdentifier:          app.Id,
			EnvironmentIdentifier:          env.Id,
			ConfigurationProfileIdentifier: profile.Id,
		})
	if err != nil {
		stop()
		t.Fatal(err)
	}

	_, err = s3Client(
		s,
		"test",
		"us-east-1",
	).CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("persistent-history-bucket")})
	if err != nil {
		stop()
		t.Fatal(err)
	}

	trail, err := cloudtrailClient(
		s,
		"test",
		"us-east-1",
	).CreateTrail(ctx,
		&cloudtrail.CreateTrailInput{
			Name:         new("persistent-trail"),
			S3BucketName: new("persistent-history-bucket"),
		})
	if err != nil {
		stop()
		t.Fatal(err)
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()
	content, err := appconfigDataClient(
		s,
		"test",
		"us-east-1",
	).GetLatestConfiguration(ctx,
		&appconfigdata.GetLatestConfigurationInput{
			ConfigurationToken: session.InitialConfigurationToken,
		})
	if err != nil || string(content.Configuration) != "persistent-content" {
		t.Fatalf("persistent data: %+v %v", content, err)
	}

	got, err := cloudtrailClient(s, "test", "us-east-1").GetTrail(ctx, &cloudtrail.GetTrailInput{Name: trail.TrailARN})
	if err != nil || aws.ToString(got.Trail.Name) != "persistent-trail" {
		t.Fatalf("persistent trail: %+v %v", got, err)
	}

	events, err := cloudtrailClient(
		s,
		"test",
		"us-east-1",
	).LookupEvents(ctx,
		&cloudtrail.LookupEventsInput{
			LookupAttributes: []types.LookupAttribute{{
				AttributeKey:   types.LookupAttributeKeyEventName,
				AttributeValue: new("CreateBucket"),
			}},
		})
	if err != nil || len(events.Events) != 1 {
		t.Fatalf("persistent history: %+v %v", events, err)
	}
}

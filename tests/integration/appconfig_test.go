package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/appconfig"
	"github.com/aws/aws-sdk-go-v2/service/appconfig/types"
	"github.com/aws/aws-sdk-go-v2/service/appconfigdata"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
)

func appconfigClient(s *httptest.Server, key, region string) *appconfig.Client {
	return appconfig.New(appconfig.Options{
		Region: region, BaseEndpoint: new(s.URL), Retryer: aws.NopRetryer{},
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: key, SecretAccessKey: "test"}, nil
		}),
	})
}

func appconfigDataClient(s *httptest.Server, key, region string) *appconfigdata.Client {
	return appconfigdata.New(appconfigdata.Options{
		Region: region, BaseEndpoint: new(s.URL), Retryer: aws.NopRetryer{},
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: key, SecretAccessKey: "test"}, nil
		}),
	})
}

func TestAppConfigHostedFlow(t *testing.T) {
	s := server(t)
	c := appconfigClient(s, "test", "us-east-1")
	data := appconfigDataClient(s, "test", "us-east-1")
	ctx := context.Background()
	app, err := c.CreateApplication(
		ctx,
		&appconfig.CreateApplicationInput{Name: new("application"), Tags: map[string]string{"a": "b"}},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateApplication(ctx, &appconfig.CreateApplicationInput{Name: new("application")})
	code(t, err, "ConflictException")
	_, err = c.GetApplication(ctx, &appconfig.GetApplicationInput{ApplicationId: new("missing")})
	code(t, err, "ResourceNotFoundException")
	for _, other := range []*appconfig.Client{
		appconfigClient(s, "test", "eu-west-1"), appconfigClient(s, "team-b", "us-east-1"),
	} {
		_, err = other.GetApplication(ctx, &appconfig.GetApplicationInput{ApplicationId: app.Id})
		code(t, err, "ResourceNotFoundException")
	}

	env, err := c.CreateEnvironment(ctx, &appconfig.CreateEnvironmentInput{ApplicationId: app.Id, Name: new("prod")})
	if err != nil {
		t.Fatal(err)
	}

	profile, err := c.CreateConfigurationProfile(ctx, &appconfig.CreateConfigurationProfileInput{
		ApplicationId: app.Id,
		Name: new(
			"config",
		),
		LocationUri: new("hosted"),
		Validators:  []types.Validator{{Type: types.ValidatorTypeJsonSchema, Content: new("{}")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, uri := range []string{"s3://bucket/file", "ssm-parameter://parameter", "secretsmanager://secret"} {
		_, err = c.CreateConfigurationProfile(
			ctx,
			&appconfig.CreateConfigurationProfileInput{
				ApplicationId: app.Id,
				Name:          new("external"),
				LocationUri:   new(uri),
			},
		)
		code(t, err, "BadRequestException")
	}

	_, err = c.CreateEnvironment(ctx, &appconfig.CreateEnvironmentInput{
		ApplicationId: app.Id, Name: new("alarm"),
		Monitors: []types.Monitor{{AlarmArn: new("arn:aws:cloudwatch:us-east-1:000000000000:alarm/test")}},
	})
	code(t, err, "NotImplemented")
	strategies, err := c.ListDeploymentStrategies(ctx, &appconfig.ListDeploymentStrategiesInput{})
	if err != nil || len(strategies.Items) != 4 {
		t.Fatalf("strategies: %+v %v", strategies, err)
	}

	for _, strategy := range strategies.Items {
		_, err = c.GetDeploymentStrategy(ctx, &appconfig.GetDeploymentStrategyInput{DeploymentStrategyId: strategy.Id})
		if err != nil {
			t.Fatal(err)
		}
	}

	v1, err := c.CreateHostedConfigurationVersion(ctx, &appconfig.CreateHostedConfigurationVersionInput{
		ApplicationId:          app.Id,
		ConfigurationProfileId: profile.Id,
		Content:                []byte{0, 1, 2, 255},
		ContentType:            new("application/octet-stream"),
		VersionLabel:           new("first"),
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := c.GetHostedConfigurationVersion(ctx, &appconfig.GetHostedConfigurationVersionInput{
		ApplicationId:          app.Id,
		ConfigurationProfileId: profile.Id, VersionNumber: new(v1.VersionNumber),
	})
	if err != nil || !bytes.Equal(got.Content, v1.Content) ||
		aws.ToString(got.ContentType) != "application/octet-stream" {
		t.Fatalf("binary hosted round trip: %+v %v", got, err)
	}

	_, err = c.CreateHostedConfigurationVersion(ctx, &appconfig.CreateHostedConfigurationVersionInput{
		ApplicationId:          app.Id,
		ConfigurationProfileId: profile.Id,
		Content:                []byte("stale"),
		ContentType:            new("text/plain"),
		LatestVersionNumber:    new(int32(0)),
	})
	code(t, err, "ConflictException")
	deployment, err := c.StartDeployment(ctx, &appconfig.StartDeploymentInput{
		ApplicationId:          app.Id,
		EnvironmentId:          env.Id,
		ConfigurationProfileId: profile.Id,
		ConfigurationVersion:   new("first"),
		DeploymentStrategyId:   new("AppConfig.AllAtOnce"),
	})
	if err != nil || deployment.State != types.DeploymentStateComplete {
		t.Fatalf("deployment: %+v %v", deployment, err)
	}

	environment, err := c.GetEnvironment(
		ctx,
		&appconfig.GetEnvironmentInput{ApplicationId: app.Id, EnvironmentId: env.Id},
	)
	if err != nil || environment.State != types.EnvironmentStateReadyForDeployment {
		t.Fatalf("environment: %+v %v", environment, err)
	}

	session, err := data.StartConfigurationSession(ctx, &appconfigdata.StartConfigurationSessionInput{
		ApplicationIdentifier: app.Id,
		EnvironmentIdentifier: new(
			"prod",
		),
		ConfigurationProfileIdentifier:       new("config"),
		RequiredMinimumPollIntervalInSeconds: new(int32(15)),
	})
	if err != nil {
		t.Fatal(err)
	}

	first, err := data.GetLatestConfiguration(
		ctx,
		&appconfigdata.GetLatestConfigurationInput{ConfigurationToken: session.InitialConfigurationToken},
	)
	if err != nil || !bytes.Equal(first.Configuration, v1.Content) || first.NextPollIntervalInSeconds != 15 {
		t.Fatalf("first poll: %+v %v", first, err)
	}

	_, err = data.GetLatestConfiguration(
		ctx,
		&appconfigdata.GetLatestConfigurationInput{ConfigurationToken: session.InitialConfigurationToken},
	)
	code(t, err, "BadRequestException")
	empty, err := data.GetLatestConfiguration(
		ctx,
		&appconfigdata.GetLatestConfigurationInput{ConfigurationToken: first.NextPollConfigurationToken},
	)
	if err != nil || len(empty.Configuration) != 0 {
		t.Fatalf("unchanged: %+v %v", empty, err)
	}

	v2, err := c.CreateHostedConfigurationVersion(ctx, &appconfig.CreateHostedConfigurationVersionInput{
		ApplicationId:          app.Id,
		ConfigurationProfileId: profile.Id,
		Content: []byte(`{
  "feature": true
}`),
		ContentType:         new("application/json"),
		LatestVersionNumber: new(v1.VersionNumber),
	})
	if err != nil {
		t.Fatal(err)
	}

	d2, err := c.StartDeployment(ctx, &appconfig.StartDeploymentInput{
		ApplicationId:          app.Id,
		EnvironmentId:          env.Id,
		ConfigurationProfileId: profile.Id,
		ConfigurationVersion:   new(fmt.Sprint(v2.VersionNumber)),
		DeploymentStrategyId:   new("AppConfig.Linear50PercentEvery30Seconds"),
	})
	if err != nil {
		t.Fatal(err)
	}

	latest, err := data.GetLatestConfiguration(
		ctx,
		&appconfigdata.GetLatestConfigurationInput{ConfigurationToken: empty.NextPollConfigurationToken},
	)
	if err != nil || !bytes.Equal(latest.Configuration, v2.Content) {
		t.Fatalf("new content: %+v %v", latest, err)
	}

	_, err = c.StopDeployment(
		ctx,
		&appconfig.StopDeploymentInput{
			ApplicationId:    app.Id,
			EnvironmentId:    env.Id,
			DeploymentNumber: new(d2.DeploymentNumber),
		},
	)
	code(t, err, "BadRequestException")
	reverted, err := c.StopDeployment(
		ctx,
		&appconfig.StopDeploymentInput{
			ApplicationId:    app.Id,
			EnvironmentId:    env.Id,
			DeploymentNumber: new(d2.DeploymentNumber),
			AllowRevert:      new(true),
		},
	)
	if err != nil || reverted.State != types.DeploymentStateReverted {
		t.Fatalf("revert: %+v %v", reverted, err)
	}

	environment, err = c.GetEnvironment(
		ctx,
		&appconfig.GetEnvironmentInput{ApplicationId: app.Id, EnvironmentId: env.Id},
	)
	if err != nil || environment.State != types.EnvironmentStateReverted {
		t.Fatalf("reverted environment: %+v %v", environment, err)
	}
	rollback, err := data.GetLatestConfiguration(
		ctx,
		&appconfigdata.GetLatestConfigurationInput{ConfigurationToken: latest.NextPollConfigurationToken},
	)
	if err != nil || !bytes.Equal(rollback.Configuration, v1.Content) {
		t.Fatalf("rollback: %+v %v", rollback, err)
	}

	for _, other := range []*appconfigdata.Client{
		appconfigDataClient(s, "test", "eu-west-1"), appconfigDataClient(s, "team-b", "us-east-1"),
	} {
		_, pollErr := other.GetLatestConfiguration(ctx, &appconfigdata.GetLatestConfigurationInput{
			ConfigurationToken: rollback.NextPollConfigurationToken,
		})
		code(t, pollErr, "BadRequestException")
	}

	appARN := "arn:aws:appconfig:us-east-1:000000000000:application/" + aws.ToString(app.Id)
	_, err = c.TagResource(
		ctx,
		&appconfig.TagResourceInput{ResourceArn: new(appARN), Tags: map[string]string{"new": "tag"}},
	)
	if err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &appconfig.ListTagsForResourceInput{ResourceArn: new(appARN)})
	if err != nil || tags.Tags["a"] != "b" || tags.Tags["new"] != "tag" {
		t.Fatalf("tags: %+v %v", tags, err)
	}

	_, err = c.UntagResource(ctx, &appconfig.UntagResourceInput{ResourceArn: new(appARN), TagKeys: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteHostedConfigurationVersion(
		ctx,
		&appconfig.DeleteHostedConfigurationVersionInput{
			ApplicationId:          app.Id,
			ConfigurationProfileId: profile.Id,
			VersionNumber:          new(v1.VersionNumber),
		},
	)
	code(t, err, "ConflictException")
	_, err = c.DeleteApplication(ctx, &appconfig.DeleteApplicationInput{ApplicationId: app.Id})
	code(t, err, "ConflictException")
	_, err = c.DeleteEnvironment(ctx, &appconfig.DeleteEnvironmentInput{ApplicationId: app.Id, EnvironmentId: env.Id})
	if err != nil {
		t.Fatal(err)
	}

	for _, number := range []int32{v1.VersionNumber, v2.VersionNumber} {
		_, err = c.DeleteHostedConfigurationVersion(
			ctx,
			&appconfig.DeleteHostedConfigurationVersionInput{
				ApplicationId:          app.Id,
				ConfigurationProfileId: profile.Id,
				VersionNumber:          new(number),
			},
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	_, err = c.DeleteConfigurationProfile(
		ctx,
		&appconfig.DeleteConfigurationProfileInput{ApplicationId: app.Id, ConfigurationProfileId: profile.Id},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteApplication(ctx, &appconfig.DeleteApplicationInput{ApplicationId: app.Id})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAppConfigCRUDAndPagination(t *testing.T) {
	s := server(t)
	c := appconfigClient(s, "test", "us-east-1")
	ctx := context.Background()
	apps := []string{}
	for _, name := range []string{"one", "two", "three"} {
		a, err := c.CreateApplication(ctx, &appconfig.CreateApplicationInput{Name: new(name)})
		if err != nil {
			t.Fatal(err)
		}

		apps = append(apps, aws.ToString(a.Id))
	}

	var token *string
	seen := map[string]bool{}
	for {
		page, err := c.ListApplications(
			ctx,
			&appconfig.ListApplicationsInput{MaxResults: new(int32(1)), NextToken: token},
		)
		if err != nil {
			t.Fatal(err)
		}

		for _, a := range page.Items {
			if seen[aws.ToString(a.Id)] {
				t.Fatal("repeated item")
			}

			seen[aws.ToString(a.Id)] = true
		}

		token = page.NextToken
		if token == nil {
			break
		}
	}

	if len(seen) != 3 {
		t.Fatalf("applications: %v", seen)
	}

	app := apps[0]
	_, err := c.UpdateApplication(ctx, &appconfig.UpdateApplicationInput{ApplicationId: new(app), Name: new("renamed")})
	if err != nil {
		t.Fatal(err)
	}

	env, err := c.CreateEnvironment(ctx, &appconfig.CreateEnvironmentInput{ApplicationId: new(app), Name: new("env")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateEnvironment(ctx, &appconfig.CreateEnvironmentInput{ApplicationId: new(app), Name: new("env")})
	code(t, err, "ConflictException")
	_, err = c.UpdateEnvironment(
		ctx,
		&appconfig.UpdateEnvironmentInput{ApplicationId: new(app), EnvironmentId: env.Id, Description: new("changed")},
	)
	if err != nil {
		t.Fatal(err)
	}

	profile, err := c.CreateConfigurationProfile(
		ctx,
		&appconfig.CreateConfigurationProfileInput{ApplicationId: new(app), Name: new("p"), LocationUri: new("hosted")},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateConfigurationProfile(
		ctx,
		&appconfig.UpdateConfigurationProfileInput{
			ApplicationId:          new(app),
			ConfigurationProfileId: profile.Id,
			Description:            new("changed"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	got, err := c.GetConfigurationProfile(
		ctx,
		&appconfig.GetConfigurationProfileInput{ApplicationId: new(app), ConfigurationProfileId: profile.Id},
	)
	if err != nil || aws.ToString(got.Description) != "changed" {
		t.Fatalf("profile: %+v %v", got, err)
	}

	strategy, err := c.CreateDeploymentStrategy(
		ctx,
		&appconfig.CreateDeploymentStrategyInput{
			Name:                        new("custom"),
			DeploymentDurationInMinutes: new(int32(2)),
			GrowthFactor:                new(float32(25)),
			ReplicateTo:                 types.ReplicateToNone,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateDeploymentStrategy(
		ctx,
		&appconfig.UpdateDeploymentStrategyInput{
			DeploymentStrategyId:   strategy.Id,
			FinalBakeTimeInMinutes: new(int32(2)),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateDeploymentStrategy(
		ctx,
		&appconfig.UpdateDeploymentStrategyInput{
			DeploymentStrategyId: new("AppConfig.AllAtOnce"),
			Description:          new("no"),
		},
	)
	code(t, err, "BadRequestException")
	_, err = c.DeleteDeploymentStrategy(
		ctx,
		&appconfig.DeleteDeploymentStrategyInput{DeploymentStrategyId: strategy.Id},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.GetDeploymentStrategy(ctx, &appconfig.GetDeploymentStrategyInput{DeploymentStrategyId: strategy.Id})
	code(t, err, "ResourceNotFoundException")
	_, err = c.ListApplications(ctx, &appconfig.ListApplicationsInput{NextToken: new("invalid!")})
	code(t, err, "BadRequestException")
}

func TestAppConfigCloudFormation(t *testing.T) {
	s := server(t)
	c := cfnClient(s)
	ctx := context.Background()
	template := `{
  "Resources": {
    "App": {
      "Type": "AWS::AppConfig::Application",
      "Properties": {
        "Name": "stack-app"
      }
    },
    "Env": {
      "Type": "AWS::AppConfig::Environment",
      "Properties": {
        "ApplicationId": {
          "Ref": "App"
        },
        "Name": "stack-env"
      }
    },
    "Profile": {
      "Type": "AWS::AppConfig::ConfigurationProfile",
      "Properties": {
        "ApplicationId": {
          "Ref": "App"
        },
        "Name": "stack-profile",
        "LocationUri": "hosted"
      }
    },
    "Version": {
      "Type": "AWS::AppConfig::HostedConfigurationVersion",
      "Properties": {
        "ApplicationId": {
          "Ref": "App"
        },
        "ConfigurationProfileId": {
          "Ref": "Profile"
        },
        "Content": "hello",
        "ContentType": "text/plain"
      }
    },
    "Strategy": {
      "Type": "AWS::AppConfig::DeploymentStrategy",
      "Properties": {
        "Name": "stack-strategy",
        "DeploymentDurationInMinutes": 0,
        "GrowthFactor": 100,
        "ReplicateTo": "NONE"
      }
    },
    "Deployment": {
      "Type": "AWS::AppConfig::Deployment",
      "Properties": {
        "ApplicationId": {
          "Ref": "App"
        },
        "EnvironmentId": {
          "Ref": "Env"
        },
        "ConfigurationProfileId": {
          "Ref": "Profile"
        },
        "ConfigurationVersion": {
          "Ref": "Version"
        },
        "DeploymentStrategyId": {
          "Ref": "Strategy"
        }
      }
    }
  }
}`
	created, err := c.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("appconfig-stack"), TemplateBody: new(template)},
	)
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, c, "appconfig-stack", "CREATE_COMPLETE")
	updated := strings.Replace(template, `"Name": "stack-app"`, `"Name": "stack-app", "Description": "updated"`, 1)
	_, err = c.UpdateStack(ctx, &cfn.UpdateStackInput{StackName: new("appconfig-stack"), TemplateBody: new(updated)})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(ctx, t, c, "appconfig-stack", "UPDATE_COMPLETE")
	_, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("appconfig-stack")})
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, c, aws.ToString(created.StackId), "DELETE_COMPLETE")
}

func TestAppConfigRawUnsupported(t *testing.T) {
	s := server(t)
	req, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		s.URL+"/applications",
		strings.NewReader(`{
  "Name": "raw",
  "Unknown": true
}`),
	)
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(
		"Authorization",
		"AWS4-HMAC-SHA256 Credential=test/20261010/us-east-1/appconfig/aws4_request, SignedHeaders=host, Signature=abc",
	)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode < 400 || !strings.Contains(string(body), "NotImplemented") {
		t.Fatalf("raw: %d %s", res.StatusCode, body)
	}
}

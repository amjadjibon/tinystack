package integration_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/appconfig"
	"github.com/aws/aws-sdk-go-v2/service/appconfig/types"
)

func TestAppConfigResourcePagination(t *testing.T) {
	server := server(t)
	c := appconfigClient(server, "test", "us-east-1")
	ctx := context.Background()
	app, err := c.CreateApplication(ctx, &appconfig.CreateApplicationInput{Name: new("pages")})
	if err != nil {
		t.Fatal(err)
	}
	var envID, profileID *string
	for i := range 3 {
		name := fmt.Sprintf("resource-%d", i)
		env, createErr := c.CreateEnvironment(
			ctx,
			&appconfig.CreateEnvironmentInput{ApplicationId: app.Id, Name: new(name)},
		)
		if createErr != nil {
			t.Fatal(createErr)
		}
		profile, createErr := c.CreateConfigurationProfile(
			ctx,
			&appconfig.CreateConfigurationProfileInput{
				ApplicationId: app.Id,
				Name:          new(name),
				LocationUri:   new("hosted"),
			},
		)
		if createErr != nil {
			t.Fatal(createErr)
		}
		_, createErr = c.CreateDeploymentStrategy(
			ctx,
			&appconfig.CreateDeploymentStrategyInput{
				Name:                        new(name),
				DeploymentDurationInMinutes: new(int32(1)),
				GrowthFactor:                new(float32(100)),
				ReplicateTo:                 types.ReplicateToNone,
			},
		)
		if createErr != nil {
			t.Fatal(createErr)
		}
		if i == 0 {
			envID = env.Id
			profileID = profile.Id
		}
	}
	for i := range 3 {
		_, createErr := c.CreateHostedConfigurationVersion(
			ctx,
			&appconfig.CreateHostedConfigurationVersionInput{
				ApplicationId:          app.Id,
				ConfigurationProfileId: profileID,
				Content:                []byte(fmt.Sprint(i)),
				ContentType:            new("text/plain"),
			},
		)
		if createErr != nil {
			t.Fatal(createErr)
		}
		_, createErr = c.StartDeployment(
			ctx,
			&appconfig.StartDeploymentInput{
				ApplicationId:          app.Id,
				EnvironmentId:          envID,
				ConfigurationProfileId: profileID,
				ConfigurationVersion:   new(fmt.Sprint(i + 1)),
				DeploymentStrategyId:   new("AppConfig.AllAtOnce"),
			},
		)
		if createErr != nil {
			t.Fatal(createErr)
		}
	}
	t.Run("ListEnvironments", func(t *testing.T) {
		var token *string
		seen := map[string]bool{}
		for {
			page, pageErr := c.ListEnvironments(
				ctx,
				&appconfig.ListEnvironmentsInput{ApplicationId: app.Id, MaxResults: new(int32(1)), NextToken: token},
			)
			if pageErr != nil {
				t.Fatal(pageErr)
			}
			for _, item := range page.Items {
				id := aws.ToString(item.Id)
				if seen[id] {
					t.Fatal("duplicate pagination item")
				}
				seen[id] = true
			}
			token = page.NextToken
			if token == nil {
				break
			}
		}
		if len(seen) != 3 {
			t.Fatalf("items: %v", seen)
		}
	})
	t.Run("ListConfigurationProfiles", func(t *testing.T) {
		var token *string
		seen := map[string]bool{}
		for {
			page, pageErr := c.ListConfigurationProfiles(
				ctx,
				&appconfig.ListConfigurationProfilesInput{
					ApplicationId: app.Id,
					MaxResults:    new(int32(1)),
					NextToken:     token,
				},
			)
			if pageErr != nil {
				t.Fatal(pageErr)
			}
			for _, item := range page.Items {
				id := aws.ToString(item.Id)
				if seen[id] {
					t.Fatal("duplicate pagination item")
				}
				seen[id] = true
			}
			token = page.NextToken
			if token == nil {
				break
			}
		}
		if len(seen) != 3 {
			t.Fatalf("items: %v", seen)
		}
	})
	t.Run("ListDeploymentStrategies", func(t *testing.T) {
		var token *string
		seen := map[string]bool{}
		for {
			page, pageErr := c.ListDeploymentStrategies(
				ctx,
				&appconfig.ListDeploymentStrategiesInput{MaxResults: new(int32(1)), NextToken: token},
			)
			if pageErr != nil {
				t.Fatal(pageErr)
			}
			for _, item := range page.Items {
				id := aws.ToString(item.Id)
				if seen[id] {
					t.Fatal("duplicate pagination item")
				}
				seen[id] = true
			}
			token = page.NextToken
			if token == nil {
				break
			}
		}
		if len(seen) != 7 {
			t.Fatalf("items: %v", seen)
		}
	})
	t.Run("ListHostedConfigurationVersions", func(t *testing.T) {
		var token *string
		seen := map[string]bool{}
		for {
			page, pageErr := c.ListHostedConfigurationVersions(
				ctx,
				&appconfig.ListHostedConfigurationVersionsInput{
					ApplicationId:          app.Id,
					ConfigurationProfileId: profileID,
					MaxResults:             new(int32(1)),
					NextToken:              token,
				},
			)
			if pageErr != nil {
				t.Fatal(pageErr)
			}
			for _, item := range page.Items {
				id := fmt.Sprint(item.VersionNumber)
				if seen[id] {
					t.Fatal("duplicate pagination item")
				}
				seen[id] = true
			}
			token = page.NextToken
			if token == nil {
				break
			}
		}
		if len(seen) != 3 {
			t.Fatalf("items: %v", seen)
		}
	})
	t.Run("ListDeployments", func(t *testing.T) {
		var token *string
		seen := map[string]bool{}
		for {
			page, pageErr := c.ListDeployments(
				ctx,
				&appconfig.ListDeploymentsInput{
					ApplicationId: app.Id,
					EnvironmentId: envID,
					MaxResults:    new(int32(1)),
					NextToken:     token,
				},
			)
			if pageErr != nil {
				t.Fatal(pageErr)
			}
			for _, item := range page.Items {
				id := fmt.Sprint(item.DeploymentNumber)
				if seen[id] {
					t.Fatal("duplicate pagination item")
				}
				seen[id] = true
			}
			token = page.NextToken
			if token == nil {
				break
			}
		}
		if len(seen) != 3 {
			t.Fatalf("items: %v", seen)
		}
	})
}

package apprunner

import (
	"context"
	"sync"
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apprunner"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestConfigurationRevisionTransactions(t *testing.T) {
	ctx := request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: "000000000000", Region: "us-east-1"}},
	)
	s := New(&memory.Store{})
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			_, err := s.CreateAutoScalingConfiguration(
				ctx,
				&api.CreateAutoScalingConfigurationRequest{AutoScalingConfigurationName: new("concurrent")},
			)
			if err != nil {
				t.Error(err)
			}
		})
	}

	wg.Wait()
	all, err := s.ListAutoScalingConfigurations(
		ctx,
		&api.ListAutoScalingConfigurationsRequest{
			AutoScalingConfigurationName: new("concurrent"),
			LatestOnly:                   new(false),
		},
	)
	if err != nil || len(all.AutoScalingConfigurationSummaryList) != 12 {
		t.Fatal(all, err)
	}

	revisions := map[int64]bool{}
	for _, v := range all.AutoScalingConfigurationSummaryList {
		revisions[common.Value(v.AutoScalingConfigurationRevision)] = true
	}

	for i := int64(1); i <= 12; i++ {
		if !revisions[i] {
			t.Fatalf("missing revision %d", i)
		}
	}

	latest, err := s.ListAutoScalingConfigurations(
		ctx,
		&api.ListAutoScalingConfigurationsRequest{AutoScalingConfigurationName: new("concurrent")},
	)
	if err != nil || len(latest.AutoScalingConfigurationSummaryList) != 1 ||
		common.Value(latest.AutoScalingConfigurationSummaryList[0].AutoScalingConfigurationRevision) != 12 {
		t.Fatal(latest, err)
	}

	// Rejected mutations must leave revisions, tags and Latest flags unchanged.
	_, err = s.CreateAutoScalingConfiguration(
		ctx,
		&api.CreateAutoScalingConfigurationRequest{
			AutoScalingConfigurationName: new("concurrent"),
			Tags:                         []api.Tag{{Key: new("aws:invalid"), Value: new("x")}},
		},
	)
	if err == nil {
		t.Fatal("reserved tag accepted")
	}

	latest, err = s.ListAutoScalingConfigurations(
		ctx,
		&api.ListAutoScalingConfigurationsRequest{AutoScalingConfigurationName: new("concurrent")},
	)
	if err != nil ||
		common.Value(latest.AutoScalingConfigurationSummaryList[0].AutoScalingConfigurationRevision) != 12 {
		t.Fatal(latest, err)
	}
}

func TestInstanceModelValues(t *testing.T) {
	check := func(c *api.InstanceConfiguration) error {
		if err := c.Validate(""); err != nil {
			return err
		}

		return checkInstance(c)
	}
	cpu := func(v string) error { return check(&api.InstanceConfiguration{Cpu: new(v)}) }
	mem := func(v string) error { return check(&api.InstanceConfiguration{Memory: new(v)}) }
	for _, v := range []string{
		"256", "512", "1024", "2048", "4096",
		"0.25 vCPU", "0.5 vCPU", "1 vCPU", "2 vCPU", "4 vCPU",
	} {
		if err := cpu(v); err != nil {
			t.Errorf("CPU %s: %v", v, err)
		}
	}

	for _, v := range []string{
		"512", "1024", "2048", "3072", "4096", "6144", "8192", "10240", "12288",
		"0.5 GB", "1 GB", "2 GB", "3 GB", "4 GB", "6 GB", "8 GB", "10 GB", "12 GB",
	} {
		if err := mem(v); err != nil {
			t.Errorf("Memory %s: %v", v, err)
		}
	}

	for _, v := range []string{"128", "8 vCPU", "1024junk"} {
		if err := cpu(v); err == nil {
			t.Errorf("CPU %q accepted", v)
		}
	}

	for _, v := range []string{"256", "5 GB", "2048junk"} {
		if err := mem(v); err == nil {
			t.Errorf("Memory %q accepted", v)
		}
	}
}

// The generated Validate methods enforce the pinned model's patterns.
func TestSourceModelPatterns(t *testing.T) {
	image := func(v string) error {
		return (&api.ImageRepository{ImageIdentifier: new(v), ImageRepositoryType: new("ECR_PUBLIC")}).Validate("")
	}
	role := func(v string) error { return (&api.AuthenticationConfiguration{AccessRoleArn: new(v)}).Validate("") }
	connection := func(v string) error {
		return (&api.CreateConnectionRequest{ConnectionName: new(v), ProviderType: new("GITHUB")}).Validate("")
	}
	directory := func(v string) error {
		return (&api.CodeRepository{
			RepositoryUrl:     new("https://github.com/example/app"),
			SourceCodeVersion: &api.SourceCodeVersion{Type: new("BRANCH"), Value: new("main")},
			SourceDirectory:   new(v),
		}).Validate("")
	}
	build := func(v string) error {
		return (&api.CodeConfigurationValues{Runtime: new("PYTHON_3"), BuildCommand: new(v)}).Validate("")
	}
	for _, test := range []struct {
		check func(string) error
		name  string
		value string
		valid bool
	}{
		{image, "public image", "public.ecr.aws/example/app:latest", true},
		{image, "private image", "123456789012.dkr.ecr.us-east-1.amazonaws.com/team/app:latest", true},
		{image, "invalid image", "not-an-image", false},
		{role, "role", "arn:aws:iam::123456789012:role/service-role/example", true},
		{role, "invalid role", "not-a-role", false},
		{connection, "name hyphen", "valid-name", true},
		{connection, "name punctuation", "bad^name", false},
		{directory, "source directory", "src/app", true},
		{directory, "source NUL", "src\x00app", false},
		{build, "build command", "go build ./...", true},
		{build, "build newline", "go build\n./...", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.check(test.value)
			if (err == nil) != test.valid {
				t.Fatalf("value %q: %v", test.value, err)
			}
		})
	}
}

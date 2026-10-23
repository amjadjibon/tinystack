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
	for _, cpu := range []string{
		"256", "512", "1024", "2048", "4096",
		"0.25 vCPU", "0.5 vCPU", "1 vCPU", "2 vCPU", "4 vCPU",
	} {
		if err := validateCpu(new(cpu)); err != nil {
			t.Errorf("CPU %s: %v", cpu, err)
		}
	}

	for _, memory := range []string{
		"512", "1024", "2048", "3072", "4096", "6144", "8192", "10240", "12288",
		"0.5 GB", "1 GB", "2 GB", "3 GB", "4 GB", "6 GB", "8 GB", "10 GB", "12 GB",
	} {
		if err := validateMemory(new(memory)); err != nil {
			t.Errorf("Memory %s: %v", memory, err)
		}
	}

	for _, cpu := range []string{"128", "8 vCPU", "1024junk"} {
		if err := validateCpu(new(cpu)); err == nil {
			t.Errorf("CPU %q accepted", cpu)
		}
	}

	for _, memory := range []string{"256", "5 GB", "2048junk"} {
		if err := validateMemory(new(memory)); err == nil {
			t.Errorf("Memory %q accepted", memory)
		}
	}
}

func TestSourceModelPatterns(t *testing.T) {
	for _, test := range []struct {
		name  string
		check func(*string) error
		value string
		valid bool
	}{
		{"public image", validateImageIdentifier, "public.ecr.aws/example/app:latest", true},
		{"private image", validateImageIdentifier, "123456789012.dkr.ecr.us-east-1.amazonaws.com/team/app:latest", true},
		{"invalid image", validateImageIdentifier, "not-an-image", false},
		{"role", validateRoleArn, "arn:aws:iam::123456789012:role/service-role/example", true},
		{"invalid role", validateRoleArn, "not-a-role", false},
		{"name hyphen", validateConnectionName, "valid-name", true},
		{"name punctuation", validateConnectionName, "bad^name", false},
		{"source directory", validateSourceDirectory, "src/app", true},
		{"source NUL", validateSourceDirectory, "src\x00app", false},
		{"build command", validateBuildCommand, "go build ./...", true},
		{"build newline", validateBuildCommand, "go build\n./...", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.check(new(test.value))
			if (err == nil) != test.valid {
				t.Fatalf("value %q: %v", test.value, err)
			}
		})
	}
}

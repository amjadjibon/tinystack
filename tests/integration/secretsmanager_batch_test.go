package integration_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

func TestSecretsManagerRandomPassword(t *testing.T) {
	s := server(t)
	c := secretsClient(s, "test", "us-east-1")
	ctx := context.Background()
	out, err := c.GetRandomPassword(ctx, &secretsmanager.GetRandomPasswordInput{})
	if err != nil || len(aws.ToString(out.RandomPassword)) != 32 {
		t.Fatalf("default %+v, %v", out, err)
	}

	for range 20 {
		out, err = c.GetRandomPassword(ctx, &secretsmanager.GetRandomPasswordInput{
			PasswordLength: aws.Int64(4), ExcludePunctuation: aws.Bool(true), IncludeSpace: aws.Bool(true),
			ExcludeCharacters: aws.String("aeiouAEIOU01"),
		})
		if err != nil {
			t.Fatal(err)
		}

		// Each type left in appears at least once.
		password := aws.ToString(out.RandomPassword)
		for _, chars := range []string{"bcdfghjklmnpqrstvwxyz", "BCDFGHJKLMNPQRSTVWXYZ", "23456789", " "} {
			if len(password) != 4 || strings.ContainsAny(password, "aeiouAEIOU01!#") ||
				!strings.ContainsAny(password, chars) {
				t.Fatalf("password %q", password)
			}
		}
	}

	for name, in := range map[string]*secretsmanager.GetRandomPasswordInput{
		"length": {PasswordLength: aws.Int64(4097)},
		"empty": {
			ExcludeLowercase: aws.Bool(true), ExcludeUppercase: aws.Bool(true), ExcludeNumbers: aws.Bool(true),
			ExcludePunctuation: aws.Bool(true),
		},
		"too short": {PasswordLength: aws.Int64(3)},
	} {
		if _, err = c.GetRandomPassword(ctx, in); err == nil {
			t.Fatalf("%s: no error", name)
		}

		code(t, err, "InvalidParameterException")
	}
}

func TestSecretsManagerBatchGetSecretValue(t *testing.T) {
	s := server(t)
	c := secretsClient(s, "test", "us-east-1")
	ctx := context.Background()
	for _, name := range []string{"app/db", "app/api", "other"} {
		if _, err := c.CreateSecret(ctx, &secretsmanager.CreateSecretInput{
			Name: aws.String(name), SecretString: aws.String("value-of-" + name),
		}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := c.CreateSecret(ctx, &secretsmanager.CreateSecretInput{Name: aws.String("app/empty")}); err != nil {
		t.Fatal(err)
	}

	out, err := c.BatchGetSecretValue(ctx, &secretsmanager.BatchGetSecretValueInput{
		SecretIdList: []string{"app/db", "missing", "other"},
	})
	if err != nil || len(out.SecretValues) != 2 || aws.ToString(out.SecretValues[1].SecretString) != "value-of-other" ||
		len(out.Errors) != 1 || aws.ToString(out.Errors[0].SecretId) != "missing" ||
		aws.ToString(out.Errors[0].ErrorCode) != "ResourceNotFoundException" {
		t.Fatalf("by ID %+v, %v", out, err)
	}

	filters := []smtypes.Filter{{Key: smtypes.FilterNameStringTypeName, Values: []string{"app/"}}}
	out, err = c.BatchGetSecretValue(ctx, &secretsmanager.BatchGetSecretValueInput{
		Filters: filters, MaxResults: aws.Int32(2),
	})
	// Secrets list by creation time, and by name within a millisecond, so the
	// first two come in either order.
	if err != nil || len(out.SecretValues) != 2 || out.NextToken == nil {
		t.Fatalf("first page %+v, %v", out, err)
	}

	names := []string{aws.ToString(out.SecretValues[0].Name), aws.ToString(out.SecretValues[1].Name)}
	slices.Sort(names)
	if !slices.Equal(names, []string{"app/api", "app/db"}) {
		t.Fatalf("first page names %v", names)
	}

	// The secret without a value is an error on the second page.
	out, err = c.BatchGetSecretValue(ctx, &secretsmanager.BatchGetSecretValueInput{
		Filters: filters, MaxResults: aws.Int32(2), NextToken: out.NextToken,
	})
	if err != nil || len(out.SecretValues) != 0 || len(out.Errors) != 1 || out.NextToken != nil {
		t.Fatalf("second page %+v, %v", out, err)
	}

	for name, in := range map[string]*secretsmanager.BatchGetSecretValueInput{
		"neither": {},
		"both":    {SecretIdList: []string{"other"}, Filters: filters},
		"paging":  {SecretIdList: []string{"other"}, MaxResults: aws.Int32(1)},
		"many":    {Filters: filters, MaxResults: aws.Int32(21)},
	} {
		if _, err = c.BatchGetSecretValue(ctx, in); err == nil {
			t.Fatalf("%s: no error", name)
		}

		code(t, err, "InvalidParameterException")
	}
}

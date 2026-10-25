package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		valid      bool
	}{
		{"defaults", "{}", true},
		{"identity", "identity:\n  accounts:\n    test: '111111111111'\n", true},
		{"bad account", "defaults:\n  account: 'bad'", false},
		{"bad mapping", "identity:\n  accounts:\n    test: 'bad'", false},
		{"verify", "identity:\n  mode: verify\n  secret_keys:\n    test: secret\n", true},
		{"verify without keys", "identity:\n  mode: verify\n", false},
		{"keys without verify", "identity:\n  secret_keys:\n    test: secret\n", false},
		{"empty secret", "identity:\n  mode: verify\n  secret_keys:\n    test: ''\n", false},
		{"bad mode", "identity:\n  mode: strict\n", false},
		{"unknown", "server:\n  typo: 10", false},
		{"persistent", "storage:\n  mode: persistent\n  path: /tmp/tinystack", true},
		{"persistent without path", "storage:\n  mode: persistent", false},
		{"memory with path", "storage:\n  path: /tmp/tinystack", false},
		{"bad storage mode", "storage:\n  mode: disk", false},
		{"docker builds", "codebuild:\n  builds: docker", true},
		{"bad builds", "codebuild:\n  builds: podman", false},
		{"docker containers", "ecs:\n  containers: docker", true},
		{"docker databases", "rds:\n  instances: docker", true},
		{"bad databases", "rds:\n  instances: podman", false},
		{"docker domains", "opensearch:\n  domains: docker", true},
		{"bad domains", "opensearch:\n  domains: podman", false},
		{"bad containers", "ecs:\n  containers: podman", false},
		{"k3s clusters", "eks:\n  clusters: k3s", true},
		{"bad clusters", "eks:\n  clusters: kind", false},
		{"port", "server:\n  port: 0", false},
		{"console off", "server:\n  console_port: 0", true},
		{"cloudshell allow", "console:\n  cloudshell:\n    allow: ['s3:*', 'sts:get-*', '*:list-*']", true},
		{"cloudshell service only", "console:\n  cloudshell:\n    allow: [s3]", false},
		{"cloudshell empty command", "console:\n  cloudshell:\n    allow: ['s3:']", false},
		{"cloudshell bad pattern", "console:\n  cloudshell:\n    allow: ['s3:[']", false},
		{"console on the AWS port", "server:\n  console_port: 4566", false},
		{"bad console port", "server:\n  console_port: 70000", false},
		{"plain mqtt", "iot:\n  mqtt_port: 1883", true},
		{"mqtt on the console port", "iot:\n  mqtt_port: 4567", false},
		{"bad mqtt port", "iot:\n  mqtt_port: -1", false},
		{
			"plain mqtt with verify",
			"identity:\n  mode: verify\n  secret_keys:\n    test: secret\niot:\n  mqtt_port: 1883", false,
		},
		{"mqtts", "iot:\n  mqtts_port: 8883", true},
		{"mqtts on the server port", "iot:\n  mqtts_port: 4566", false},
		{"mqtts on the console port", "iot:\n  mqtts_port: 4567", false},
		{"mqtts on the mqtt port", "iot:\n  mqtts_port: 1883\n  mqtt_port: 1883", false},
		{"bad mqtts port", "iot:\n  mqtts_port: 70000", false},
		{
			"mqtts with verify",
			"identity:\n  mode: verify\n  secret_keys:\n    test: secret\niot:\n  mqtts_port: 8883", true,
		},
		{"documents", "{}\n---\n{}", false},
		{
			"valid accounts",
			`accounts:
  "123456789012":
    name: "Production"
    created_date: "2024-01-01T00:00:00Z"
    state: "ACTIVE"
    contact:
      full_name: "Admin"
      address_line1: "123 Main St"
      city: "Seattle"
      state_or_region: "WA"
      postal_code: "98101"
      country_code: "US"
      phone_number: "+12065550100"
    regions:
      af-south-1: "ENABLED"
`,
			true,
		},
		{
			"bad account id key",
			`accounts:
  "bad-id":
    name: "Bad"
`,
			false,
		},
		{
			"bad account created date",
			`accounts:
  "123456789012":
    created_date: "invalid-date"
`,
			false,
		},
		{
			"bad account state",
			`accounts:
  "123456789012":
    state: "UNKNOWN_STATE"
`,
			false,
		},
		{
			"bad region opt status",
			`accounts:
  "123456789012":
    regions:
      us-east-1: "BAD_STATUS"
`,
			false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if guardErr := os.WriteFile(path, []byte(tc.text), 0o600); guardErr != nil {
				t.Fatal(guardErr)
			}

			_, err := Load(path)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
}

func TestAccountContactFixtureRejectsMissingRequiredFields(t *testing.T) {
	c := Default()
	c.Accounts["123456789012"] = AccountConfig{Contact: &AccountContactConfig{}}
	if err := c.Validate(); err == nil {
		t.Fatal("empty contact fixture should fail instead of publishing an invalid ContactInformation record")
	}
}

func TestOmittedContactAllowed(t *testing.T) {
	c := Default()
	c.Accounts["123456789012"] = AccountConfig{
		Name:  "No Contact Account",
		State: "ACTIVE",
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("omitted contact should be allowed: %v", err)
	}
}

func TestAccountContactValidation(t *testing.T) {
	validContact := func() AccountContactConfig {
		return AccountContactConfig{
			FullName:      "Jane Doe",
			AddressLine1:  "123 Main St",
			City:          "Seattle",
			StateOrRegion: "WA",
			PostalCode:    "98101",
			CountryCode:   "US",
			PhoneNumber:   "+12065550100",
		}
	}

	tests := []struct {
		name      string
		modify    func(c *AccountContactConfig)
		expectErr string
	}{
		{
			name:   "valid minimal required fixture",
			modify: func(c *AccountContactConfig) {},
		},
		{
			name: "valid full fixture with optional fields",
			modify: func(c *AccountContactConfig) {
				c.AddressLine2 = "Suite 400"
				c.AddressLine3 = "Building 2"
				c.DistrictOrCounty = "King"
				c.CompanyName = "Acme Corp"
				c.WebsiteURL = "https://example.com"
			},
		},
		{
			name: "valid unicode characters in name and address",
			modify: func(c *AccountContactConfig) {
				c.FullName = "山田 太郎"
				c.AddressLine1 = "東京都千代田区千代田1-1"
				c.City = "千代田区"
				c.CountryCode = "JP"
				c.StateOrRegion = "東京都"
				c.PostalCode = "100-8111"
				c.PhoneNumber = "+81-3-1234-5678"
			},
		},
		{
			name: "valid non-conditional country without state_or_region",
			modify: func(c *AccountContactConfig) {
				c.CountryCode = "FR"
				c.StateOrRegion = ""
				c.City = "Paris"
				c.PostalCode = "75001"
				c.PhoneNumber = "+33123456789"
			},
		},
		{
			name: "missing full_name",
			modify: func(c *AccountContactConfig) {
				c.FullName = ""
			},
			expectErr: "full_name is required",
		},
		{
			name: "overlong full_name",
			modify: func(c *AccountContactConfig) {
				c.FullName = strings.Repeat("a", 51)
			},
			expectErr: "full_name is required and must be between 1 and 50 characters",
		},
		{
			name: "missing address_line1",
			modify: func(c *AccountContactConfig) {
				c.AddressLine1 = ""
			},
			expectErr: "address_line1 is required",
		},
		{
			name: "overlong address_line1",
			modify: func(c *AccountContactConfig) {
				c.AddressLine1 = strings.Repeat("b", 61)
			},
			expectErr: "address_line1 is required and must be between 1 and 60 characters",
		},
		{
			name: "overlong address_line2",
			modify: func(c *AccountContactConfig) {
				c.AddressLine2 = strings.Repeat("c", 61)
			},
			expectErr: "address_line2 must be at most 60 characters",
		},
		{
			name: "overlong address_line3",
			modify: func(c *AccountContactConfig) {
				c.AddressLine3 = strings.Repeat("d", 61)
			},
			expectErr: "address_line3 must be at most 60 characters",
		},
		{
			name: "missing city",
			modify: func(c *AccountContactConfig) {
				c.City = ""
			},
			expectErr: "city is required",
		},
		{
			name: "overlong city",
			modify: func(c *AccountContactConfig) {
				c.City = strings.Repeat("e", 51)
			},
			expectErr: "city is required and must be between 1 and 50 characters",
		},
		{
			name: "missing country_code",
			modify: func(c *AccountContactConfig) {
				c.CountryCode = ""
			},
			expectErr: "country_code is required",
		},
		{
			name: "invalid country_code length",
			modify: func(c *AccountContactConfig) {
				c.CountryCode = "USA"
			},
			expectErr: "2-letter ISO-3166 country code",
		},
		{
			name: "invalid country_code non-letters",
			modify: func(c *AccountContactConfig) {
				c.CountryCode = "12"
			},
			expectErr: "2-letter ISO-3166 country code",
		},
		{
			name: "missing state_or_region for US",
			modify: func(c *AccountContactConfig) {
				c.CountryCode = "US"
				c.StateOrRegion = ""
			},
			expectErr: "state_or_region is required for country US",
		},
		{
			name: "missing state_or_region for CA",
			modify: func(c *AccountContactConfig) {
				c.CountryCode = "CA"
				c.StateOrRegion = ""
			},
			expectErr: "state_or_region is required for country CA",
		},
		{
			name: "missing state_or_region for GB",
			modify: func(c *AccountContactConfig) {
				c.CountryCode = "GB"
				c.StateOrRegion = ""
			},
			expectErr: "state_or_region is required for country GB",
		},
		{
			name: "missing state_or_region for DE",
			modify: func(c *AccountContactConfig) {
				c.CountryCode = "DE"
				c.StateOrRegion = ""
			},
			expectErr: "state_or_region is required for country DE",
		},
		{
			name: "missing state_or_region for JP",
			modify: func(c *AccountContactConfig) {
				c.CountryCode = "JP"
				c.StateOrRegion = ""
			},
			expectErr: "state_or_region is required for country JP",
		},
		{
			name: "missing state_or_region for IN",
			modify: func(c *AccountContactConfig) {
				c.CountryCode = "IN"
				c.StateOrRegion = ""
			},
			expectErr: "state_or_region is required for country IN",
		},
		{
			name: "missing state_or_region for BR",
			modify: func(c *AccountContactConfig) {
				c.CountryCode = "BR"
				c.StateOrRegion = ""
			},
			expectErr: "state_or_region is required for country BR",
		},
		{
			name: "overlong state_or_region",
			modify: func(c *AccountContactConfig) {
				c.StateOrRegion = strings.Repeat("s", 51)
			},
			expectErr: "state_or_region must be at most 50 characters",
		},
		{
			name: "overlong district_or_county",
			modify: func(c *AccountContactConfig) {
				c.DistrictOrCounty = strings.Repeat("x", 51)
			},
			expectErr: "district_or_county must be at most 50 characters",
		},
		{
			name: "missing postal_code",
			modify: func(c *AccountContactConfig) {
				c.PostalCode = ""
			},
			expectErr: "postal_code is required",
		},
		{
			name: "overlong postal_code",
			modify: func(c *AccountContactConfig) {
				c.PostalCode = strings.Repeat("p", 21)
			},
			expectErr: "postal_code is required and must be between 1 and 20 characters",
		},
		{
			name: "missing phone_number",
			modify: func(c *AccountContactConfig) {
				c.PhoneNumber = ""
			},
			expectErr: "phone_number is required",
		},
		{
			name: "invalid phone_number missing leading plus",
			modify: func(c *AccountContactConfig) {
				c.PhoneNumber = "12065550100"
			},
			expectErr: "phone_number is required, must be between 1 and 20 characters, and must match format",
		},
		{
			name: "invalid phone_number letters",
			modify: func(c *AccountContactConfig) {
				c.PhoneNumber = "+1206555abcd"
			},
			expectErr: "phone_number is required, must be between 1 and 20 characters, and must match format",
		},
		{
			name: "overlong phone_number",
			modify: func(c *AccountContactConfig) {
				c.PhoneNumber = "+123456789012345678901"
			},
			expectErr: "phone_number is required, must be between 1 and 20 characters, and must match format",
		},
		{
			name: "overlong company_name",
			modify: func(c *AccountContactConfig) {
				c.CompanyName = strings.Repeat("c", 51)
			},
			expectErr: "company_name must be at most 50 characters",
		},
		{
			name: "overlong website_url",
			modify: func(c *AccountContactConfig) {
				c.WebsiteURL = strings.Repeat("w", 257)
			},
			expectErr: "website_url must be at most 256 characters",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			contact := validContact()
			tt.modify(&contact)
			err := contact.Validate()
			if tt.expectErr == "" {
				if err != nil {
					t.Fatalf("expected valid contact, got: %v", err)
				}
			} else {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.expectErr)
				}
				if !strings.Contains(err.Error(), tt.expectErr) {
					t.Fatalf("expected error containing %q, got: %v", tt.expectErr, err)
				}
			}
		})
	}
}

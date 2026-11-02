package integration_test

import (
	"net/http"
	"strings"
	"testing"
)

func TestConsoleSESv2Lifecycle(t *testing.T) {
	srv := server(t)
	const account, region = "000000000000", "eu-west-1"
	run := func(operation string, input map[string]any, status int) map[string]any {
		t.Helper()
		return consoleResponse(t, srv,
			consoleLambdaRequest(t, srv, "sesv2", operation, account, region, input), status)
	}
	call := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		out, _ := run(operation, input, http.StatusOK)["output"].(map[string]any)
		return out
	}

	var capabilities map[string]any
	if internal(t, srv, http.MethodGet, consoleActionsPath, &capabilities) != http.StatusOK ||
		capabilities["sesv2"] != true {
		t.Fatal(capabilities)
	}

	// Identities: an address and a domain, with tags.
	call("CreateEmailIdentity", map[string]any{
		"EmailIdentity": "sender@example.com",
		"Tags":          []any{map[string]any{"Key": "team", "Value": "console"}},
	})

	domain := call("CreateEmailIdentity", map[string]any{"EmailIdentity": "example.org"})
	if domain["IdentityType"] != "DOMAIN" {
		t.Fatal(domain)
	}

	identity := call("GetEmailIdentity", map[string]any{"EmailIdentity": "example.org"})
	if tokens, _ := identity["DkimAttributes"].(map[string]any)["Tokens"].([]any); len(tokens) != 3 {
		t.Fatal(identity)
	}

	if len(call("ListEmailIdentities", map[string]any{"PageSize": 100})["EmailIdentities"].([]any)) != 2 {
		t.Fatal("identities not listed")
	}

	identityARN := "arn:aws:ses:" + region + ":" + account + ":identity/sender@example.com"
	call("TagResource", map[string]any{
		"ResourceArn": identityARN, "Tags": []any{map[string]any{"Key": "env", "Value": "dev"}},
	})
	if len(call("ListTagsForResource", map[string]any{"ResourceArn": identityARN})["Tags"].([]any)) != 2 {
		t.Fatal("tags not listed")
	}

	call("UntagResource", map[string]any{"ResourceArn": identityARN, "TagKeys": []string{"env"}})

	// Configuration sets and their event destinations.
	call("CreateConfigurationSet", map[string]any{
		"ConfigurationSetName": "console-set",
		"SendingOptions":       map[string]any{"SendingEnabled": true},
		"DeliveryOptions":      map[string]any{"TlsPolicy": "REQUIRE"},
	})

	set := call("GetConfigurationSet", map[string]any{"ConfigurationSetName": "console-set"})
	if set["DeliveryOptions"].(map[string]any)["TlsPolicy"] != "REQUIRE" {
		t.Fatal(set)
	}

	topic := "arn:aws:sns:" + region + ":" + account + ":events"
	call("CreateConfigurationSetEventDestination", map[string]any{
		"ConfigurationSetName": "console-set", "EventDestinationName": "to-sns",
		"EventDestination": map[string]any{
			"Enabled": true, "MatchingEventTypes": []string{"SEND"},
			"SnsDestination": map[string]any{"TopicArn": topic},
		},
	})
	call("UpdateConfigurationSetEventDestination", map[string]any{
		"ConfigurationSetName": "console-set", "EventDestinationName": "to-sns",
		"EventDestination": map[string]any{
			"Enabled": false, "MatchingEventTypes": []string{"SEND", "BOUNCE"},
			"SnsDestination": map[string]any{"TopicArn": topic},
		},
	})

	destinations := call("GetConfigurationSetEventDestinations",
		map[string]any{"ConfigurationSetName": "console-set"})["EventDestinations"].([]any)
	if len(destinations) != 1 || destinations[0].(map[string]any)["Enabled"] == true ||
		len(destinations[0].(map[string]any)["MatchingEventTypes"].([]any)) != 2 {
		t.Fatal(destinations)
	}

	// A destination with no target fails and is not stored.
	run("CreateConfigurationSetEventDestination", map[string]any{
		"ConfigurationSetName": "console-set", "EventDestinationName": "empty",
		"EventDestination": map[string]any{"Enabled": true, "MatchingEventTypes": []string{"SEND"}},
	}, http.StatusBadRequest)

	// Templates: create, edit, render.
	call("CreateEmailTemplate", map[string]any{
		"TemplateName": "welcome",
		"TemplateContent": map[string]any{
			"Subject": "Hi {{name}}",
			"Text":    "Hello {{name}}",
			"Html":    "<p>{{name}}</p>",
		},
	})
	call("UpdateEmailTemplate", map[string]any{
		"TemplateName":    "welcome",
		"TemplateContent": map[string]any{"Subject": "Welcome {{name}}", "Text": "Hello {{name}}"},
	})

	got := call("GetEmailTemplate", map[string]any{"TemplateName": "welcome"})["TemplateContent"].(map[string]any)
	if got["Subject"] != "Welcome {{name}}" {
		t.Fatal(got)
	}

	rendered := call("TestRenderEmailTemplate", map[string]any{
		"TemplateName": "welcome", "TemplateData": `{"name":"Ada"}`,
	})["RenderedTemplate"].(string)
	if !strings.Contains(rendered, "Welcome Ada") {
		t.Fatal(rendered)
	}

	if len(call("ListEmailTemplates", map[string]any{"PageSize": 100})["TemplatesMetadata"].([]any)) != 1 {
		t.Fatal("templates not listed")
	}

	// Sending: simple content through a set, then a stored template.
	sent := call("SendEmail", map[string]any{
		"FromEmailAddress": "sender@example.com", "ConfigurationSetName": "console-set",
		"Destination": map[string]any{"ToAddresses": []string{"to@example.com"}},
		"Content": map[string]any{"Simple": map[string]any{
			"Subject": map[string]any{"Data": "Test"},
			"Body":    map[string]any{"Text": map[string]any{"Data": "Body"}},
		}},
	})
	if sent["MessageId"] == "" || sent["MessageId"] == nil {
		t.Fatal(sent)
	}

	call("SendEmail", map[string]any{
		"FromEmailAddress": "sender@example.com",
		"Destination":      map[string]any{"ToAddresses": []string{"to@example.com"}},
		"Content": map[string]any{"Template": map[string]any{
			"TemplateName": "welcome", "TemplateData": `{"name":"Ada"}`,
		}},
	})

	info := call("GetAccount", map[string]any{})
	if info["SendingEnabled"] != true || info["SendQuota"].(map[string]any)["SentLast24Hours"] != float64(2) {
		t.Fatal(info)
	}

	// An unverified sender is rejected.
	run("SendEmail", map[string]any{
		"FromEmailAddress": "nobody@example.net",
		"Destination":      map[string]any{"ToAddresses": []string{"to@example.com"}},
		"Content": map[string]any{"Simple": map[string]any{
			"Subject": map[string]any{"Data": "x"}, "Body": map[string]any{"Text": map[string]any{"Data": "x"}},
		}},
	}, http.StatusBadRequest)

	// Deletes.
	call("DeleteConfigurationSetEventDestination", map[string]any{
		"ConfigurationSetName": "console-set", "EventDestinationName": "to-sns",
	})
	call("DeleteConfigurationSet", map[string]any{"ConfigurationSetName": "console-set"})
	call("DeleteEmailTemplate", map[string]any{"TemplateName": "welcome"})
	call("DeleteEmailIdentity", map[string]any{"EmailIdentity": "sender@example.com"})
	call("DeleteEmailIdentity", map[string]any{"EmailIdentity": "example.org"})

	if len(call("ListEmailIdentities", map[string]any{})["EmailIdentities"].([]any)) != 0 {
		t.Fatal("identities not deleted")
	}

	// Operations the console does not offer stay out of reach.
	for _, op := range []string{"SendBulkEmail", "PutAccountDetails", "CreateContactList"} {
		run(op, map[string]any{}, http.StatusBadRequest)
	}
}

package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

func TestConsoleCloudFrontLifecycle(t *testing.T) {
	srv := server(t)
	const account = "000000000000"
	// CloudFront is global: console actions carry no Region.
	send := func(status int, operation string, input map[string]any) map[string]any {
		t.Helper()
		return consoleResponse(t, srv,
			consoleLambdaRequest(t, srv, "cloudfront", operation, account, "", input), status)
	}
	call := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		return send(http.StatusOK, operation, input)["output"].(map[string]any)
	}
	items := func(out map[string]any, list string) []any {
		t.Helper()
		items, _ := out[list].(map[string]any)["Items"].([]any)
		return items
	}
	tags := func(arn string) map[string]string {
		t.Helper()
		got := map[string]string{}
		for _, tag := range items(call("ListTagsForResource", map[string]any{"Resource": arn}), "Tags") {
			got[tag.(map[string]any)["Key"].(string)] = tag.(map[string]any)["Value"].(string)
		}

		return got
	}

	var capabilities map[string]any
	if internal(t, srv, http.MethodGet, consoleActionsPath, &capabilities) != http.StatusOK ||
		!consolePage(capabilities, "cloudfront") {
		t.Fatal(capabilities)
	}

	// An origin access control, then a distribution that uses it.
	oac := call("CreateOriginAccessControl", map[string]any{
		"OriginAccessControlConfig": map[string]any{
			"Name": "assets", "Description": "bucket access", "OriginAccessControlOriginType": "s3",
			"SigningBehavior": "always", "SigningProtocol": "sigv4",
		},
	})["OriginAccessControl"].(map[string]any)
	oacID := oac["Id"].(string)

	created := call("CreateDistributionWithTags", map[string]any{
		"DistributionConfigWithTags": map[string]any{
			"DistributionConfig": map[string]any{
				"CallerReference": "console-1", "Comment": "Web", "Enabled": true,
				"Aliases":           map[string]any{"Quantity": 1, "Items": []any{"www.example.com"}},
				"DefaultRootObject": "",
				"Origins": map[string]any{"Quantity": 2, "Items": []any{
					map[string]any{
						"Id": "assets", "DomainName": "assets.s3.us-east-1.amazonaws.com",
						"S3OriginConfig": map[string]any{"OriginAccessIdentity": ""}, "OriginAccessControlId": oacID,
					},
					map[string]any{
						"Id": "api", "DomainName": "api.example.com", "OriginPath": "/v1",
						"CustomOriginConfig": map[string]any{
							"HTTPPort": 8080, "HTTPSPort": 443, "OriginProtocolPolicy": "http-only",
						},
					},
				}},
				"DefaultCacheBehavior": map[string]any{
					"TargetOriginId": "api", "ViewerProtocolPolicy": "redirect-to-https", "Compress": true,
					"AllowedMethods": map[string]any{
						"Quantity": 2, "Items": []any{"GET", "HEAD"},
						"CachedMethods": map[string]any{"Quantity": 2, "Items": []any{"GET", "HEAD"}},
					},
					"CachePolicyId": "658327ea-f89d-4fab-a63d-7e88639e58f6",
				},
			},
			"Tags": map[string]any{"Items": []any{map[string]any{"Key": "team", "Value": "web"}}},
		},
	})
	distribution := created["Distribution"].(map[string]any)
	id, arn := distribution["Id"].(string), distribution["ARN"].(string)
	if distribution["Status"] != "Deployed" || created["ETag"] == "" {
		t.Fatal(created)
	}

	// The list carries what the page shows, with times as ISO strings.
	listed := items(call("ListDistributions", map[string]any{"MaxItems": 100}), "DistributionList")
	if len(listed) != 1 {
		t.Fatal(listed)
	}

	row := listed[0].(map[string]any)
	if _, err := time.Parse(time.RFC3339Nano, row["LastModifiedTime"].(string)); err != nil ||
		row["Enabled"] != true || len(row["Origins"].(map[string]any)["Items"].([]any)) != 2 {
		t.Fatal(row)
	}

	if got := tags(arn); len(got) != 1 || got["team"] != "web" {
		t.Fatal(got)
	}

	// An update needs the ETag of the latest version, and a disabled distribution can be deleted.
	latest := call("GetDistributionConfig", map[string]any{"Id": id})
	config, etag := latest["DistributionConfig"].(map[string]any), latest["ETag"].(string)
	config["Enabled"] = false
	config["Comment"] = "Web (off)"
	response := send(http.StatusBadRequest, "UpdateDistribution", map[string]any{
		"Id": id, "IfMatch": "stale", "DistributionConfig": config,
	})
	if response["code"] != "InvalidIfMatchVersion" {
		t.Fatal(response)
	}

	response = send(http.StatusConflict, "DeleteDistribution", map[string]any{"Id": id, "IfMatch": etag})
	if response["code"] != "DistributionNotDisabled" {
		t.Fatal(response)
	}

	updated := call("UpdateDistribution", map[string]any{"Id": id, "IfMatch": etag, "DistributionConfig": config})
	if updated["ETag"] == etag {
		t.Fatal("the update kept the ETag")
	}

	got := call("GetDistribution", map[string]any{"Id": id})["Distribution"].(map[string]any)
	if got["DistributionConfig"].(map[string]any)["Enabled"] != false ||
		got["DistributionConfig"].(map[string]any)["Comment"] != "Web (off)" {
		t.Fatal(got)
	}

	// An invalidation lists with its paths and leaves the distribution list intact.
	invalidation := call("CreateInvalidation", map[string]any{
		"DistributionId": id,
		"InvalidationBatch": map[string]any{
			"CallerReference": "console-invalidation",
			"Paths":           map[string]any{"Quantity": 2, "Items": []any{"/index.html", "/images/*"}},
		},
	})["Invalidation"].(map[string]any)
	invalidations := items(call("ListInvalidations", map[string]any{"DistributionId": id, "MaxItems": 100}),
		"InvalidationList")
	if len(invalidations) != 1 || invalidations[0].(map[string]any)["Id"] != invalidation["Id"] {
		t.Fatal(invalidations)
	}

	fetched := call("GetInvalidation", map[string]any{"DistributionId": id, "Id": invalidation["Id"]})
	paths := fetched["Invalidation"].(map[string]any)["InvalidationBatch"].(map[string]any)["Paths"].(map[string]any)
	if len(paths["Items"].([]any)) != 2 {
		t.Fatal(fetched)
	}

	if len(items(call("ListDistributions", map[string]any{}), "DistributionList")) != 1 {
		t.Fatal("the invalidation hid the distribution")
	}

	// Tags change in place.
	call("TagResource", map[string]any{
		"Resource": arn, "Tags": map[string]any{"Items": []any{map[string]any{"Key": "env", "Value": "dev"}}},
	})
	call("UntagResource", map[string]any{"Resource": arn, "TagKeys": map[string]any{"Items": []any{"team"}}})
	if got := tags(arn); len(got) != 1 || got["env"] != "dev" {
		t.Fatal(got)
	}

	// An access control that an origin uses stays until the distribution is gone.
	current := call("GetOriginAccessControlConfig", map[string]any{"Id": oacID})
	response = send(http.StatusConflict, "DeleteOriginAccessControl", map[string]any{
		"Id": oacID, "IfMatch": current["ETag"],
	})
	if response["code"] != "OriginAccessControlInUse" {
		t.Fatal(response)
	}

	call("UpdateOriginAccessControl", map[string]any{
		"Id": oacID, "IfMatch": current["ETag"],
		"OriginAccessControlConfig": map[string]any{
			"Name": "assets-renamed", "OriginAccessControlOriginType": "s3",
			"SigningBehavior": "never", "SigningProtocol": "sigv4",
		},
	})
	summary := items(call("ListOriginAccessControls", map[string]any{"MaxItems": 100}), "OriginAccessControlList")
	if len(summary) != 1 || summary[0].(map[string]any)["Name"] != "assets-renamed" ||
		summary[0].(map[string]any)["SigningBehavior"] != "never" {
		t.Fatal(summary)
	}

	call("DeleteDistribution", map[string]any{"Id": id, "IfMatch": updated["ETag"]})
	renewed := call("GetOriginAccessControl", map[string]any{"Id": oacID})
	call("DeleteOriginAccessControl", map[string]any{"Id": oacID, "IfMatch": renewed["ETag"]})
	if len(items(call("ListDistributions", map[string]any{}), "DistributionList")) != 0 ||
		len(items(call("ListOriginAccessControls", map[string]any{}), "OriginAccessControlList")) != 0 {
		t.Fatal("resources remain")
	}

	// A key value store moves from provisioning to ready, keeps tags, and changes only with its ETag.
	store := call("CreateKeyValueStore", map[string]any{
		"Name": "redirects", "Comment": "first",
		"Tags": map[string]any{"Items": []any{map[string]any{"Key": "team", "Value": "edge"}}},
	})["KeyValueStore"].(map[string]any)
	if store["Status"] != "PROVISIONING" {
		t.Fatal(store)
	}

	stores := items(call("ListKeyValueStores", map[string]any{"MaxItems": 100}), "KeyValueStoreList")
	if len(stores) != 1 || stores[0].(map[string]any)["Status"] != "READY" {
		t.Fatal(stores)
	}

	if got := tags(store["ARN"].(string)); len(got) != 1 || got["team"] != "edge" {
		t.Fatal(got)
	}

	described := call("DescribeKeyValueStore", map[string]any{"Name": "redirects"})
	response = send(http.StatusPreconditionFailed, "UpdateKeyValueStore", map[string]any{
		"Name": "redirects", "IfMatch": "stale", "Comment": "second",
	})
	if response["code"] != "PreconditionFailed" {
		t.Fatal(response)
	}

	call("UpdateKeyValueStore", map[string]any{"Name": "redirects", "IfMatch": described["ETag"], "Comment": "second"})
	described = call("DescribeKeyValueStore", map[string]any{"Name": "redirects"})
	if described["KeyValueStore"].(map[string]any)["Comment"] != "second" {
		t.Fatal(described)
	}

	call("DeleteKeyValueStore", map[string]any{"Name": "redirects", "IfMatch": described["ETag"]})
	if len(items(call("ListKeyValueStores", map[string]any{}), "KeyValueStoreList")) != 0 {
		t.Fatal("store remains")
	}

	// A distribution created without tags sends an empty tag list.
	bare := call("CreateDistributionWithTags", map[string]any{
		"DistributionConfigWithTags": map[string]any{
			"DistributionConfig": map[string]any{
				"CallerReference": "console-2", "Comment": "", "Enabled": false,
				"Origins": map[string]any{"Quantity": 1, "Items": []any{map[string]any{
					"Id": "site", "DomainName": "site.example.com",
					"CustomOriginConfig": map[string]any{
						"HTTPPort": 80, "HTTPSPort": 443, "OriginProtocolPolicy": "https-only",
					},
				}}},
				"DefaultCacheBehavior": map[string]any{"TargetOriginId": "site", "ViewerProtocolPolicy": "allow-all"},
			},
			"Tags": map[string]any{"Items": []any{}},
		},
	})["Distribution"].(map[string]any)
	if got := tags(bare["ARN"].(string)); len(got) != 0 {
		t.Fatal(got)
	}

	// The page needs only tagged creation; other implemented operations stay out of the console.
	send(http.StatusBadRequest, "CreateDistribution", map[string]any{})
}

func TestConsoleCloudFrontSignaturesAndScope(t *testing.T) {
	srv := verifyingServer(t)
	for _, account := range []string{"000000000000", "111111111111"} {
		r := consoleLambdaRequest(t, srv, "cloudfront", "ListDistributions", account, "", map[string]any{})
		body, err := r.GetBody()
		if err != nil {
			t.Fatal(err)
		}

		raw, err := io.ReadAll(body)
		body.Close()
		if err != nil {
			t.Fatal(err)
		}

		sum := sha256.Sum256(raw)
		if err := v4.NewSigner().SignHTTP(context.Background(), aws.Credentials{
			AccessKeyID: "test", SecretAccessKey: "test",
		}, r, hex.EncodeToString(sum[:]), "cloudfront", "us-east-1", time.Now()); err != nil {
			t.Fatal(err)
		}

		// Global actions are signed in us-east-1 whatever Region the page is in; the account still has to match.
		status := http.StatusOK
		if account != "000000000000" {
			status = http.StatusForbidden
		}

		consoleResponse(t, srv, r, status)
	}

	consoleResponse(t, srv, consoleLambdaRequest(t, srv, "cloudfront", "ListDistributions",
		"000000000000", "", map[string]any{}), http.StatusForbidden)
}

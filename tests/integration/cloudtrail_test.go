package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/cloudtrail"
	"github.com/aws/aws-sdk-go-v2/service/cloudtrail/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

func cloudtrailClient(s *httptest.Server, key, region string) *cloudtrail.Client {
	return cloudtrail.New(cloudtrail.Options{
		Region: region, BaseEndpoint: new(s.URL), Retryer: aws.NopRetryer{},
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: key, SecretAccessKey: "test"}, nil
		}),
	})
}

func TestCloudTrailLifecycle(t *testing.T) {
	s := server(t)
	c := cloudtrailClient(s, "test", "us-east-1")
	ctx := context.Background()
	buckets := s3Client(s, "test", "us-east-1")
	_, err := c.CreateTrail(ctx, &cloudtrail.CreateTrailInput{Name: new("trail"), S3BucketName: new("missing-bucket")})
	code(t, err, "S3BucketDoesNotExistException")
	for _, name := range []string{"trail-bucket", "trail-bucket-new"} {
		_, err = buckets.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new(name)})
		if err != nil {
			t.Fatal(err)
		}
	}

	trail, err := c.CreateTrail(ctx, &cloudtrail.CreateTrailInput{
		Name: new("trail"), S3BucketName: new("trail-bucket"),
		TagsList: []types.Tag{{Key: new("initial"), Value: new("yes")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateTrail(ctx, &cloudtrail.CreateTrailInput{Name: new("trail"), S3BucketName: new("trail-bucket")})
	code(t, err, "TrailAlreadyExistsException")
	_, err = c.GetTrail(ctx, &cloudtrail.GetTrailInput{Name: new("missing")})
	code(t, err, "TrailNotFoundException")
	for _, other := range []*cloudtrail.Client{
		cloudtrailClient(s, "test", "eu-west-1"), cloudtrailClient(s, "team-b", "us-east-1"),
	} {
		_, err = other.GetTrail(ctx, &cloudtrail.GetTrailInput{Name: trail.TrailARN})
		code(t, err, "TrailNotFoundException")
	}

	_, err = c.UpdateTrail(
		ctx,
		&cloudtrail.UpdateTrailInput{
			Name:         trail.TrailARN,
			S3BucketName: new("trail-bucket-new"),
			S3KeyPrefix:  new("logs/"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	got, err := c.GetTrail(ctx, &cloudtrail.GetTrailInput{Name: trail.TrailARN})
	if err != nil || aws.ToString(got.Trail.S3BucketName) != "trail-bucket-new" {
		t.Fatalf("trail: %+v %v", got, err)
	}

	described, err := c.DescribeTrails(ctx, &cloudtrail.DescribeTrailsInput{TrailNameList: []string{"trail"}})
	if err != nil || len(described.TrailList) != 1 {
		t.Fatalf("describe: %+v %v", described, err)
	}

	_, err = c.StartLogging(ctx, &cloudtrail.StartLoggingInput{Name: trail.TrailARN})
	if err != nil {
		t.Fatal(err)
	}

	status, err := c.GetTrailStatus(ctx, &cloudtrail.GetTrailStatusInput{Name: trail.TrailARN})
	if err != nil || !aws.ToBool(status.IsLogging) || status.StartLoggingTime == nil {
		t.Fatalf("status: %+v %v", status, err)
	}

	_, err = c.StopLogging(ctx, &cloudtrail.StopLoggingInput{Name: trail.TrailARN})
	if err != nil {
		t.Fatal(err)
	}

	status, err = c.GetTrailStatus(ctx, &cloudtrail.GetTrailStatusInput{Name: trail.TrailARN})
	if err != nil || aws.ToBool(status.IsLogging) || status.StopLoggingTime == nil {
		t.Fatalf("stopped: %+v %v", status, err)
	}

	_, err = c.PutEventSelectors(ctx, &cloudtrail.PutEventSelectorsInput{
		TrailName: trail.TrailARN,
		EventSelectors: []types.EventSelector{
			{ReadWriteType: types.ReadWriteTypeWriteOnly, IncludeManagementEvents: new(true)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	selectors, err := c.GetEventSelectors(ctx, &cloudtrail.GetEventSelectorsInput{TrailName: trail.TrailARN})
	if err != nil || len(selectors.EventSelectors) != 1 ||
		selectors.EventSelectors[0].ReadWriteType != types.ReadWriteTypeWriteOnly {
		t.Fatalf("selectors: %+v %v", selectors, err)
	}

	_, err = c.PutEventSelectors(ctx, &cloudtrail.PutEventSelectorsInput{
		TrailName: trail.TrailARN,
		AdvancedEventSelectors: []types.AdvancedEventSelector{
			{Name: new("management"), FieldSelectors: []types.AdvancedFieldSelector{
				{Field: new("eventCategory"), Equals: []string{"Management"}},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	selectors, err = c.GetEventSelectors(ctx, &cloudtrail.GetEventSelectorsInput{TrailName: trail.TrailARN})
	if err != nil || len(selectors.AdvancedEventSelectors) != 1 || len(selectors.EventSelectors) != 0 {
		t.Fatalf("advanced: %+v %v", selectors, err)
	}

	_, err = c.AddTags(
		ctx,
		&cloudtrail.AddTagsInput{
			ResourceId: trail.TrailARN,
			TagsList:   []types.Tag{{Key: new("new"), Value: new("tag")}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTags(ctx, &cloudtrail.ListTagsInput{ResourceIdList: []string{aws.ToString(trail.TrailARN)}})
	if err != nil || len(tags.ResourceTagList) != 1 || len(tags.ResourceTagList[0].TagsList) != 2 {
		t.Fatalf("tags: %+v %v", tags, err)
	}

	_, err = c.RemoveTags(
		ctx,
		&cloudtrail.RemoveTagsInput{ResourceId: trail.TrailARN, TagsList: []types.Tag{{Key: new("initial")}}},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateTrail(ctx, &cloudtrail.UpdateTrailInput{Name: trail.TrailARN, KmsKeyId: new("unsupported")})
	code(t, err, "ValidationException")
	_, err = c.CreateTrail(
		ctx,
		&cloudtrail.CreateTrailInput{
			Name:               new("unsupported"),
			S3BucketName:       new("trail-bucket"),
			IsMultiRegionTrail: new(true),
		},
	)
	code(t, err, "ValidationException")
	_, err = c.DeleteTrail(ctx, &cloudtrail.DeleteTrailInput{Name: trail.TrailARN})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.GetTrail(ctx, &cloudtrail.GetTrailInput{Name: trail.TrailARN})
	code(t, err, "TrailNotFoundException")
}

func TestCloudTrailLookupEvents(t *testing.T) {
	s := server(t)
	c := cloudtrailClient(s, "test", "us-east-1")
	ctx := context.Background()
	buckets := s3Client(s, "test", "us-east-1")
	start := time.Now().Add(-time.Second)
	for i := range 3 {
		_, err := buckets.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new(fmt.Sprintf("history-bucket-%d", i))})
		if err != nil {
			t.Fatal(err)
		}
	}

	_, err := buckets.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("INVALID BUCKET")})
	if err == nil {
		t.Fatal("duplicate bucket succeeded")
	}

	_, err = secretsClient(
		s,
		"test",
		"us-east-1",
	).CreateSecret(ctx,
		&secretsmanager.CreateSecretInput{
			Name:         new("history-secret"),
			SecretString: new("never-record-this-password"),
		})
	if err != nil {
		t.Fatal(err)
	}

	end := time.Now().Add(time.Second)
	in := &cloudtrail.LookupEventsInput{
		LookupAttributes: []types.LookupAttribute{
			{AttributeKey: types.LookupAttributeKeyEventName, AttributeValue: new("CreateBucket")},
		},
		MaxResults: new(int32(1)),
		StartTime:  new(start),
		EndTime:    new(end),
	}

	seen := map[string]bool{}
	failure := false
	for {
		page, lookupErr := c.LookupEvents(ctx, in)
		if lookupErr != nil {
			t.Fatal(lookupErr)
		}

		for _, ev := range page.Events {
			id := aws.ToString(ev.EventId)
			if seen[id] {
				t.Fatal("pagination repeated event")
			}

			seen[id] = true
			if aws.ToString(ev.EventSource) != "s3.amazonaws.com" || aws.ToString(ev.Username) != "test" ||
				aws.ToString(ev.ReadOnly) != "false" {
				t.Fatalf("event: %+v", ev)
			}

			var document map[string]any
			if lookupErr = json.Unmarshal([]byte(aws.ToString(ev.CloudTrailEvent)), &document); lookupErr != nil {
				t.Fatal(lookupErr)
			}

			if _, ok := document["requestParameters"]; ok {
				t.Fatal("parameters were recorded")
			}

			if document["eventVersion"] != "1.09" || document["sourceIPAddress"] == "" || document["userAgent"] == "" {
				t.Fatalf("document: %v", document)
			}

			if document["errorCode"] != nil {
				failure = true
			}
		}

		in.NextToken = page.NextToken
		if in.NextToken == nil {
			break
		}
	}

	if len(seen) != 4 || !failure {
		t.Fatalf("history: %d events, failure %v", len(seen), failure)
	}

	for key, value := range map[types.LookupAttributeKey]string{
		types.LookupAttributeKeyEventSource: "s3.amazonaws.com", types.LookupAttributeKeyUsername: "test",
		types.LookupAttributeKeyReadOnly: "false", types.LookupAttributeKeyResourceName: "history-bucket-1",
		types.LookupAttributeKeyResourceType: "AWS::S3::Bucket",
	} {
		page, lookupErr := c.LookupEvents(
			ctx,
			&cloudtrail.LookupEventsInput{
				LookupAttributes: []types.LookupAttribute{{AttributeKey: key, AttributeValue: new(value)}},
			},
		)
		if lookupErr != nil || len(page.Events) == 0 {
			t.Fatalf("filter %s: %+v %v", key, page, lookupErr)
		}
	}

	all, err := c.LookupEvents(ctx, &cloudtrail.LookupEventsInput{})
	if err != nil {
		t.Fatal(err)
	}

	for _, ev := range all.Events {
		if strings.Contains(aws.ToString(ev.CloudTrailEvent), "never-record-this-password") {
			t.Fatal("secret leaked")
		}
	}

	for _, other := range []*cloudtrail.Client{
		cloudtrailClient(s, "test", "eu-west-1"), cloudtrailClient(s, "team-b", "us-east-1"),
	} {
		page, lookupErr := other.LookupEvents(ctx, &cloudtrail.LookupEventsInput{})
		if lookupErr != nil || len(page.Events) != 0 {
			t.Fatalf("isolated: %+v %v", page, lookupErr)
		}
	}

	_, err = c.LookupEvents(ctx, &cloudtrail.LookupEventsInput{NextToken: new("invalid!")})
	code(t, err, "InvalidNextTokenException")
	_, err = c.LookupEvents(
		ctx,
		&cloudtrail.LookupEventsInput{
			LookupAttributes: []types.LookupAttribute{
				{AttributeKey: types.LookupAttributeKeyEventName, AttributeValue: new("CreateBucket")},
				{AttributeKey: types.LookupAttributeKeyUsername, AttributeValue: new("test")},
			},
		},
	)
	code(t, err, "InvalidLookupAttributesException")
	// Console calls use catalog.Caller and must not add management events.
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		s.URL+"/_tinystack/actions",
		strings.NewReader(
			`{
  "service": "s3",
  "operation": "CreateBucket",
  "input": {
    "Bucket": "console-bucket"
  },
  "region": "us-east-1",
  "account": "000000000000"
}`,
		),
	)
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("console: %d %s", res.StatusCode, body)
	}

	page, err := c.LookupEvents(
		ctx,
		&cloudtrail.LookupEventsInput{
			LookupAttributes: []types.LookupAttribute{
				{AttributeKey: types.LookupAttributeKeyResourceName, AttributeValue: new("console-bucket")},
			},
		},
	)
	if err != nil || len(page.Events) != 0 {
		t.Fatalf("console leaked: %+v %v", page, err)
	}

	req, err = http.NewRequestWithContext(ctx, http.MethodPost, s.URL+"/_tinystack/reset", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	res.Body.Close()
	page, err = c.LookupEvents(ctx, &cloudtrail.LookupEventsInput{})
	if err != nil || len(page.Events) != 0 {
		t.Fatalf("reset history: %+v %v", page, err)
	}
}

func TestCloudTrailCloudFormation(t *testing.T) {
	s := server(t)
	c := cfnClient(s)
	ctx := context.Background()
	created, err := c.CreateStack(
		ctx,
		&cfn.CreateStackInput{
			StackName: new("trail-stack"),
			TemplateBody: new(
				`{
  "Resources": {
    "Bucket": {
      "Type": "AWS::S3::Bucket",
      "Properties": {
        "BucketName": "stack-trail-bucket"
      }
    },
    "Trail": {
      "Type": "AWS::CloudTrail::Trail",
      "Properties": {
        "TrailName": "stack-trail",
        "S3BucketName": {
          "Ref": "Bucket"
        },
        "IsLogging": true,
        "Tags": [
          {
            "Key": "stack",
            "Value": "yes"
          }

        ]
      }
    }
  }
}`,
			),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, c, "trail-stack", "CREATE_COMPLETE")
	events, err := cloudtrailClient(s, "test", "us-east-1").LookupEvents(ctx, &cloudtrail.LookupEventsInput{
		LookupAttributes: []types.LookupAttribute{
			{AttributeKey: types.LookupAttributeKeyEventName, AttributeValue: new("CreateBucket")},
		},
	})
	if err != nil || len(events.Events) != 0 {
		t.Fatalf("internal CF calls leaked: %+v %v", events, err)
	}

	status, err := cloudtrailClient(
		s,
		"test",
		"us-east-1",
	).GetTrailStatus(ctx, &cloudtrail.GetTrailStatusInput{Name: new("stack-trail")})
	if err != nil || !aws.ToBool(status.IsLogging) {
		t.Fatalf("stack logging: %+v %v", status, err)
	}

	_, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("trail-stack")})
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, c, aws.ToString(created.StackId), "DELETE_COMPLETE")
}

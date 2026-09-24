package integration_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// tooMany is one tag over the per-resource limit.
func tooMany() map[string]string {
	tags := map[string]string{}
	for i := range 51 {
		tags[fmt.Sprintf("k%d", i)] = "v"
	}

	return tags
}

func TestSQSTags(t *testing.T) {
	s := server(t)
	c := sqsClient(s, "test")
	ctx := context.Background()
	created, err := c.CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName: aws.String("tagged"), Tags: map[string]string{"team": "core"},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.TagQueue(ctx, &sqs.TagQueueInput{QueueUrl: created.QueueUrl, Tags: map[string]string{"env": "dev"}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UntagQueue(ctx, &sqs.UntagQueueInput{QueueUrl: created.QueueUrl, TagKeys: []string{"team"}})
	if err != nil {
		t.Fatal(err)
	}

	listed, err := c.ListQueueTags(ctx, &sqs.ListQueueTagsInput{QueueUrl: created.QueueUrl})
	if err != nil || len(listed.Tags) != 1 || listed.Tags["env"] != "dev" {
		t.Fatal(listed, err)
	}

	_, err = c.TagQueue(ctx, &sqs.TagQueueInput{QueueUrl: created.QueueUrl, Tags: tooMany()})
	code(t, err, "InvalidParameterValue")

	// AWS Query clients send tags as flattened Tag.N.Key and Tag.N.Value pairs.
	form := url.Values{
		"Action": {"ListQueueTags"}, "Version": {"2012-11-05"}, "QueueUrl": {*created.QueueUrl},
	}

	resp, err := http.Post(s.URL, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "<Tag><Key>env</Key><Value>dev</Value></Tag>") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

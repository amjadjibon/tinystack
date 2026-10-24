package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	sdk "github.com/aws/aws-sdk-go-v2/service/translate"
	"github.com/aws/aws-sdk-go-v2/service/translate/types"
)

func mlFixture(t *testing.T, s *httptest.Server, method, path, body string, status int) {
	t.Helper()
	ctx := context.Background()
	req, err := http.NewRequestWithContext(ctx, method, s.URL+"/_tinystack/"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	sum := sha256.Sum256([]byte(body))
	err = v4.NewSigner().SignHTTP(
		ctx, aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"},
		req, hex.EncodeToString(sum[:]), "tinystack", "us-east-1", time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}

	response, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != status {
		t.Fatalf("%s %s: %d %s %v", method, path, response.StatusCode, data, err)
	}
}

func translateClient(s *httptest.Server) *sdk.Client {
	return sdk.New(
		sdk.Options{
			Region:       "us-east-1",
			BaseEndpoint: new(s.URL),
			Credentials:  creds("test"),
			Retryer:      aws.NopRetryer{},
		},
	)
}

func TestTranslateFixtures(t *testing.T) {
	t.Parallel()
	s := server(t)
	c := translateClient(s)
	ctx := context.Background()
	input := &sdk.TranslateTextInput{SourceLanguageCode: new("en"), TargetLanguageCode: new("fr"), Text: new("hello")}
	_, err := c.TranslateText(ctx, input)
	wantAccessAnalyzerError(t, err, "InvalidRequestException")
	mlFixture(
		t,
		s,
		"PUT",
		"translate/fixtures/hello",
		`{"sourceLanguageCode":"en","targetLanguageCode":"fr","text":"hello","translatedText":"bonjour"}`,
		200,
	)
	mlFixture(
		t,
		s,
		"PUT",
		"translate/fixtures/missing-answer",
		`{"sourceLanguageCode":"en","targetLanguageCode":"fr","text":"hello"}`,
		400,
	)
	mlFixture(t, s, "GET", "translate/fixtures/hello", "", 200)
	mlFixture(t, s, "GET", "translate/fixtures", "", 200)
	out, err := c.TranslateText(ctx, input)
	if err != nil || aws.ToString(out.TranslatedText) != "bonjour" {
		t.Fatal(out, err)
	}

	input.SourceLanguageCode = new("auto")
	out, err = c.TranslateText(ctx, input)
	if err != nil || aws.ToString(out.SourceLanguageCode) != "en" {
		t.Fatal(out, err)
	}

	mlFixture(
		t,
		s,
		"PUT",
		"translate/fixtures/ambiguous",
		`{"sourceLanguageCode":"de","targetLanguageCode":"fr","text":"hello","translatedText":"fixture"}`,
		200,
	)
	_, err = c.TranslateText(ctx, input)
	wantAccessAnalyzerError(t, err, "InvalidRequestException")
	mlFixture(t, s, "DELETE", "translate/fixtures/ambiguous", "", 204)
	input.Settings = &types.TranslationSettings{Formality: types.FormalityFormal}
	_, err = c.TranslateText(ctx, input)
	wantAccessAnalyzerError(t, err, "InvalidRequestException")
	doc, err := c.TranslateDocument(
		ctx,
		&sdk.TranslateDocumentInput{
			Document:           &types.Document{Content: []byte("hello"), ContentType: new("text/plain")},
			SourceLanguageCode: new("en"),
			TargetLanguageCode: new("fr"),
		},
	)
	if err != nil || string(doc.TranslatedDocument.Content) != "bonjour" {
		t.Fatal(doc, err)
	}

	languages, err := c.ListLanguages(ctx, &sdk.ListLanguagesInput{MaxResults: new(int32(5))})
	if err != nil || len(languages.Languages) != 5 || languages.NextToken == nil {
		t.Fatal(languages, err)
	}

	next, err := c.ListLanguages(ctx, &sdk.ListLanguagesInput{NextToken: languages.NextToken})
	if err != nil || len(next.Languages) != 70 {
		t.Fatal(next, err)
	}

	_, err = c.ListLanguages(ctx, &sdk.ListLanguagesInput{DisplayLanguageCode: types.DisplayLanguageCodeFr})
	wantAccessAnalyzerError(t, err, "InvalidRequestException")
	mlFixture(t, s, "DELETE", "translate/fixtures/hello", "", 204)
	_, err = c.TranslateText(
		ctx,
		&sdk.TranslateTextInput{SourceLanguageCode: new("en"), TargetLanguageCode: new("fr"), Text: new("hello")},
	)
	wantAccessAnalyzerError(t, err, "InvalidRequestException")
}

func TestTranslateDataAndBatch(t *testing.T) {
	t.Parallel()
	s := server(t)
	c := translateClient(s)
	ctx := context.Background()
	term, err := c.ImportTerminology(
		ctx,
		&sdk.ImportTerminologyInput{
			Name:          new("terms"),
			MergeStrategy: types.MergeStrategyOverwrite,
			TerminologyData: &types.TerminologyData{
				File:   []byte("en,fr\nhello,bonjour\n"),
				Format: types.TerminologyDataFormatCsv,
			},
			Tags: []types.Tag{{Key: new("team"), Value: new("core")}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	arn := term.TerminologyProperties.Arn
	_, err = c.GetTerminology(
		ctx,
		&sdk.GetTerminologyInput{Name: new("terms"), TerminologyDataFormat: types.TerminologyDataFormatCsv},
	)
	wantAccessAnalyzerError(t, err, "InvalidRequestException")
	terms, err := c.ListTerminologies(ctx, &sdk.ListTerminologiesInput{})
	if err != nil || len(terms.TerminologyPropertiesList) != 1 {
		t.Fatal(terms, err)
	}

	_, err = c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: arn, Tags: []types.Tag{{Key: new("env"), Value: new("test")}}},
	)
	if err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: arn})
	if err != nil || len(tags.Tags) != 2 {
		t.Fatal(tags, err)
	}

	_, err = c.UntagResource(ctx, &sdk.UntagResourceInput{ResourceArn: arn, TagKeys: []string{"env"}})
	if err != nil {
		t.Fatal(err)
	}

	objects := s3Client(s, "test", "us-east-1")
	_, err = objects.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("translate-input")})
	if err != nil {
		t.Fatal(err)
	}

	for key, text := range map[string]string{
		"data.csv":    "en,fr\nhello,bonjour\n",
		"input/a.txt": "hello",
		"input/b.txt": "missing",
	} {
		_, err = objects.PutObject(
			ctx,
			&s3.PutObjectInput{Bucket: new("translate-input"), Key: new(key), Body: strings.NewReader(text)},
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	cfg := &types.ParallelDataConfig{S3Uri: new("s3://translate-input/data.csv"), Format: types.ParallelDataFormatCsv}
	_, err = c.CreateParallelData(ctx, &sdk.CreateParallelDataInput{Name: new("parallel"), ParallelDataConfig: cfg})
	if err != nil {
		t.Fatal(err)
	}

	pollML(t, func() bool {
		out, e := c.GetParallelData(ctx, &sdk.GetParallelDataInput{Name: new("parallel")})
		return e == nil && out.ParallelDataProperties.Status == types.ParallelDataStatusActive
	})
	_, err = c.UpdateParallelData(ctx, &sdk.UpdateParallelDataInput{Name: new("parallel"), ParallelDataConfig: cfg})
	if err != nil {
		t.Fatal(err)
	}

	parallel, err := c.ListParallelData(ctx, &sdk.ListParallelDataInput{})
	if err != nil || len(parallel.ParallelDataPropertiesList) != 1 {
		t.Fatal(parallel, err)
	}

	roles := iamClient(s, "test", "us-east-1")
	role, err := roles.CreateRole(
		ctx,
		&iam.CreateRoleInput{
			RoleName:                 new("translator"),
			AssumeRolePolicyDocument: new(`{"Version":"2012-10-17","Statement":[]}`),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	mlFixture(
		t,
		s,
		"PUT",
		"translate/fixtures/hello",
		`{"sourceLanguageCode":"en","targetLanguageCode":"fr","text":"hello","translatedText":"bonjour"}`,
		200,
	)
	jobInput := &sdk.StartTextTranslationJobInput{
		JobName:           new("batch"),
		ClientToken:       new("batch-token"),
		DataAccessRoleArn: role.Role.Arn,
		InputDataConfig: &types.InputDataConfig{
			S3Uri:       new("s3://translate-input/input/"),
			ContentType: new("text/plain"),
		},
		OutputDataConfig:    &types.OutputDataConfig{S3Uri: new("s3://translate-input/output/")},
		SourceLanguageCode:  new("en"),
		TargetLanguageCodes: []string{"fr"},
	}
	job, err := c.StartTextTranslationJob(ctx, jobInput)
	if err != nil {
		t.Fatal(err)
	}

	var properties *types.TextTranslationJobProperties
	pollML(t, func() bool {
		out, e := c.DescribeTextTranslationJob(ctx, &sdk.DescribeTextTranslationJobInput{JobId: job.JobId})
		if e != nil {
			return false
		}

		properties = out.TextTranslationJobProperties
		return properties.JobStatus == types.JobStatusCompletedWithError
	})
	key := strings.TrimPrefix(aws.ToString(properties.OutputDataConfig.S3Uri), "s3://translate-input/") + "fr.a.txt"
	output, err := objects.GetObject(ctx, &s3.GetObjectInput{Bucket: new("translate-input"), Key: new(key)})
	if err != nil {
		t.Fatal(err)
	}

	data, err := io.ReadAll(output.Body)
	output.Body.Close()
	if err != nil || string(data) != "bonjour" {
		t.Fatal(string(data), err)
	}

	listed, err := c.ListTextTranslationJobs(
		ctx,
		&sdk.ListTextTranslationJobsInput{Filter: &types.TextTranslationJobFilter{JobName: new("batch")}},
	)
	if err != nil || len(listed.TextTranslationJobPropertiesList) != 1 {
		t.Fatal(listed, err)
	}

	jobInput.ClientToken = new("stop-token")
	stopped, err := c.StartTextTranslationJob(ctx, jobInput)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.StopTextTranslationJob(ctx, &sdk.StopTextTranslationJobInput{JobId: stopped.JobId})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteParallelData(ctx, &sdk.DeleteParallelDataInput{Name: new("parallel")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteTerminology(ctx, &sdk.DeleteTerminologyInput{Name: new("terms")})
	if err != nil {
		t.Fatal(err)
	}
}

func pollML(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatal("ML worker did not reach expected state")
}

func TestTranslatePaginationAndSharedTags(t *testing.T) {
	t.Parallel()
	s := server(t)
	ctx := context.Background()
	c := translateClient(s)
	for _, name := range []string{"z-last", "a-first"} {
		_, err := c.ImportTerminology(
			ctx,
			&sdk.ImportTerminologyInput{
				Name:          new(name),
				MergeStrategy: types.MergeStrategyOverwrite,
				TerminologyData: &types.TerminologyData{
					Format: types.TerminologyDataFormatTsv,
					File:   []byte("en\tfr\nhello\tbonjour\n"),
				},
			},
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	page, err := c.ListTerminologies(ctx, &sdk.ListTerminologiesInput{MaxResults: new(int32(1))})
	if err != nil || len(page.TerminologyPropertiesList) != 1 ||
		aws.ToString(page.TerminologyPropertiesList[0].Name) != "a-first" ||
		page.NextToken == nil {
		t.Fatal(page, err)
	}

	next, err := c.ListTerminologies(
		ctx,
		&sdk.ListTerminologiesInput{MaxResults: new(int32(1)), NextToken: page.NextToken},
	)
	if err != nil || len(next.TerminologyPropertiesList) != 1 ||
		aws.ToString(next.TerminologyPropertiesList[0].Name) != "z-last" {
		t.Fatal(next, err)
	}

	_, err = c.ListTerminologies(ctx, &sdk.ListTerminologiesInput{NextToken: new("%%%")})
	wantAccessAnalyzerError(t, err, "InvalidRequestException")
	tags := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	resource := aws.ToString(page.TerminologyPropertiesList[0].Arn)
	result, err := tags.TagResources(
		ctx,
		&rgta.TagResourcesInput{ResourceARNList: []string{resource}, Tags: map[string]string{"shared": "yes"}},
	)
	if err != nil || len(result.FailedResourcesMap) != 0 {
		t.Fatal(result, err)
	}

	tagged, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: new(resource)})
	if err != nil || len(tagged.Tags) != 1 || aws.ToString(tagged.Tags[0].Value) != "yes" {
		t.Fatal(tagged, err)
	}
}

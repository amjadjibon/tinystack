package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	sdk "github.com/aws/aws-sdk-go-v2/service/transcribe"
	"github.com/aws/aws-sdk-go-v2/service/transcribe/types"
	"github.com/aws/aws-sdk-go-v2/service/translate"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

func transcribeClient(s *httptest.Server) *sdk.Client {
	return sdk.New(
		sdk.Options{
			Region:       "us-east-1",
			BaseEndpoint: new(s.URL),
			Credentials:  creds("test"),
			Retryer:      aws.NopRetryer{},
		},
	)
}

func TestTranscribeFixturesAndVocabularies(t *testing.T) {
	t.Parallel()
	s := server(t)
	c := transcribeClient(s)
	ctx := context.Background()
	objects := s3Client(s, "test", "us-east-1")
	_, err := objects.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("transcribe-media")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = objects.PutObject(
		ctx,
		&s3.PutObjectInput{
			Bucket: new("transcribe-media"),
			Key:    new("audio.wav"),
			Body:   strings.NewReader("fixture audio bytes"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateVocabulary(
		ctx,
		&sdk.CreateVocabularyInput{
			VocabularyName: new("words"),
			LanguageCode:   types.LanguageCodeEnUs,
			Phrases:        []string{"TinyStack"},
			Tags:           []types.Tag{{Key: new("team"), Value: new("core")}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	pollML(t, func() bool {
		out, e := c.GetVocabulary(ctx, &sdk.GetVocabularyInput{VocabularyName: new("words")})
		return e == nil && out.VocabularyState == types.VocabularyStateReady && out.DownloadUri == nil
	})
	_, err = c.UpdateVocabulary(
		ctx,
		&sdk.UpdateVocabularyInput{
			VocabularyName: new("words"),
			LanguageCode:   types.LanguageCodeEnUs,
			Phrases:        []string{"fixture"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	pollML(t, func() bool {
		out, e := c.GetVocabulary(ctx, &sdk.GetVocabularyInput{VocabularyName: new("words")})
		return e == nil && out.VocabularyState == types.VocabularyStateReady
	})
	vocab, err := c.ListVocabularies(
		ctx,
		&sdk.ListVocabulariesInput{NameContains: new("words"), StateEquals: types.VocabularyStateReady},
	)
	if err != nil || len(vocab.Vocabularies) != 1 {
		t.Fatal(vocab, err)
	}

	_, err = c.CreateVocabularyFilter(
		ctx,
		&sdk.CreateVocabularyFilterInput{
			VocabularyFilterName: new("filter"),
			LanguageCode:         types.LanguageCodeEnUs,
			Words:                []string{"example"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateVocabularyFilter(
		ctx,
		&sdk.UpdateVocabularyFilterInput{VocabularyFilterName: new("filter"), Words: []string{"updated"}},
	)
	if err != nil {
		t.Fatal(err)
	}

	filter, err := c.GetVocabularyFilter(ctx, &sdk.GetVocabularyFilterInput{VocabularyFilterName: new("filter")})
	if err != nil || filter.DownloadUri != nil {
		t.Fatal(filter, err)
	}

	filters, err := c.ListVocabularyFilters(ctx, &sdk.ListVocabularyFiltersInput{NameContains: new("filter")})
	if err != nil || len(filters.VocabularyFilters) != 1 {
		t.Fatal(filters, err)
	}

	for _, arn := range []string{
		"arn:aws:transcribe:us-east-1:000000000000:vocabulary/words",
		"arn:aws:transcribe:us-east-1:000000000000:vocabulary-filter/filter",
	} {
		_, err = c.TagResource(
			ctx,
			&sdk.TagResourceInput{ResourceArn: new(arn), Tags: []types.Tag{{Key: new("env"), Value: new("test")}}},
		)
		if err != nil {
			t.Fatal(err)
		}

		tags, e := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: new(arn)})
		if e != nil || len(tags.Tags) == 0 {
			t.Fatal(tags, e)
		}

		_, err = c.UntagResource(ctx, &sdk.UntagResourceInput{ResourceArn: new(arn), TagKeys: []string{"env"}})
		if err != nil {
			t.Fatal(err)
		}
	}

	input := &sdk.StartTranscriptionJobInput{
		TranscriptionJobName: new("missing"),
		LanguageCode:         types.LanguageCodeEnUs,
		Media:                &types.Media{MediaFileUri: new("s3://transcribe-media/audio.wav")},
		OutputBucketName:     new("transcribe-media"),
		OutputKey:            new("results/"),
		Settings: &types.Settings{
			VocabularyName:         new("words"),
			VocabularyFilterName:   new("filter"),
			VocabularyFilterMethod: types.VocabularyFilterMethodRemove,
		},
	}
	_, err = c.StartTranscriptionJob(ctx, input)
	if err != nil {
		t.Fatal(err)
	}

	pollML(t, func() bool {
		out, e := c.GetTranscriptionJob(ctx, &sdk.GetTranscriptionJobInput{TranscriptionJobName: new("missing")})
		return e == nil && out.TranscriptionJob.TranscriptionJobStatus == types.TranscriptionJobStatusFailed &&
			strings.Contains(aws.ToString(out.TranscriptionJob.FailureReason), "/_tinystack/transcribe/fixtures")
	})
	mlFixture(
		t,
		s,
		"PUT",
		"transcribe/fixtures/audio",
		`{"mediaFileUri":"s3://transcribe-media/audio.wav","languageCode":"en-US","transcript":"Fixture transcript."}`,
		200,
	)
	mlFixture(
		t,
		s,
		"PUT",
		"transcribe/fixtures/missing-answer",
		`{"mediaFileUri":"s3://transcribe-media/audio.wav","languageCode":"en-US"}`,
		400,
	)
	mlFixture(t, s, "GET", "transcribe/fixtures/audio", "", 200)
	input.TranscriptionJobName = new("registered")
	_, err = c.StartTranscriptionJob(ctx, input)
	if err != nil {
		t.Fatal(err)
	}

	pollML(t, func() bool {
		out, e := c.GetTranscriptionJob(ctx, &sdk.GetTranscriptionJobInput{TranscriptionJobName: new("registered")})
		return e == nil && out.TranscriptionJob.TranscriptionJobStatus == types.TranscriptionJobStatusCompleted &&
			aws.ToString(
				out.TranscriptionJob.Transcript.TranscriptFileUri,
			) == "s3://transcribe-media/results/registered.json"
	})
	output, err := objects.GetObject(
		ctx,
		&s3.GetObjectInput{Bucket: new("transcribe-media"), Key: new("results/registered.json")},
	)
	if err != nil {
		t.Fatal(err)
	}

	data, err := io.ReadAll(output.Body)
	output.Body.Close()
	var doc map[string]any
	if err != nil || json.Unmarshal(data, &doc) != nil {
		t.Fatal(string(data), err)
	}

	results := doc["results"].(map[string]any)
	if len(results) != 1 || results["transcripts"].([]any)[0].(map[string]any)["transcript"] != "Fixture transcript." {
		t.Fatal(doc)
	}

	list, err := c.ListTranscriptionJobs(
		ctx,
		&sdk.ListTranscriptionJobsInput{
			Status:          types.TranscriptionJobStatusCompleted,
			JobNameContains: new("registered"),
			MaxResults:      new(int32(1)),
		},
	)
	if err != nil || len(list.TranscriptionJobSummaries) != 1 {
		t.Fatal(list, err)
	}

	input.TranscriptionJobName = new("unsupported")
	input.IdentifyLanguage = new(true)
	_, err = c.StartTranscriptionJob(ctx, input)
	if err == nil {
		t.Fatal("IdentifyLanguage was accepted")
	}

	input.IdentifyLanguage = nil
	input.OutputBucketName = nil
	_, err = c.StartTranscriptionJob(ctx, input)
	wantAccessAnalyzerError(t, err, "BadRequestException")
	for _, name := range []string{"missing", "registered"} {
		_, err = c.DeleteTranscriptionJob(ctx, &sdk.DeleteTranscriptionJobInput{TranscriptionJobName: new(name)})
		if err != nil {
			t.Fatal(err)
		}
	}

	_, err = c.DeleteVocabulary(ctx, &sdk.DeleteVocabularyInput{VocabularyName: new("words")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteVocabularyFilter(ctx, &sdk.DeleteVocabularyFilterInput{VocabularyFilterName: new("filter")})
	if err != nil {
		t.Fatal(err)
	}

	// Transcribe reports unknown jobs and vocabularies as BadRequestException, unknown filters as NotFoundException.
	_, err = c.GetVocabulary(ctx, &sdk.GetVocabularyInput{VocabularyName: new("words")})
	wantAccessAnalyzerError(t, err, "BadRequestException")
	_, err = c.GetTranscriptionJob(ctx, &sdk.GetTranscriptionJobInput{TranscriptionJobName: new("registered")})
	wantAccessAnalyzerError(t, err, "BadRequestException")
	_, err = c.GetVocabularyFilter(ctx, &sdk.GetVocabularyFilterInput{VocabularyFilterName: new("filter")})
	wantAccessAnalyzerError(t, err, "NotFoundException")
	mlFixture(t, s, "DELETE", "transcribe/fixtures/audio", "", 204)
}

func TestTranscribeAndTranslateSQLiteRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	mlFixture(
		t,
		s,
		"PUT",
		"translate/fixtures/restart",
		`{"sourceLanguageCode":"en","targetLanguageCode":"fr","text":"restart","translatedText":"redémarrer"}`,
		200,
	)
	mlFixture(
		t,
		s,
		"PUT",
		"transcribe/fixtures/restart",
		`{"mediaFileUri":"s3://media/audio.wav","languageCode":"en-US","transcript":"persisted"}`,
		200,
	)
	stop()
	// Seed the exact durable state left by a process dying after its transactional claim.
	store, err := sqlite.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}

	scope := request.Scope{AccountID: "000000000000", Region: "us-east-1"}
	err = store.Update(ctx, func(tx storage.WriteTx) error {
		for service, kind := range map[string]string{"translate": "job", "transcribe": "transcription-job"} {
			row := map[string]any{"Properties": map[string]any{"JobId": "interrupted", "JobStatus": "IN_PROGRESS"}}
			if service == "transcribe" {
				row = map[string]any{
					"Name":  "interrupted",
					"State": "IN_PROGRESS",
					"Job": map[string]any{
						"TranscriptionJobName":   "interrupted",
						"TranscriptionJobStatus": "IN_PROGRESS",
					},
				}
			}

			if e := storage.Write(tx, scope.Key(service, kind, "interrupted"), row); e != nil {
				return e
			}

			if e := storage.Write(
				tx,
				"aws/"+service+"/index/"+scope.AccountID+"/"+scope.Region+"/"+kind+"/interrupted",
				map[string]any{"Scope": scope, "Kind": kind, "Name": "interrupted"},
			); e != nil {
				return e
			}
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if closeErr := store.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	s, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()
	translated, err := translateClient(
		s,
	).TranslateText(ctx, &translate.TranslateTextInput{
		SourceLanguageCode: new("en"), TargetLanguageCode: new("fr"), Text: new("restart"),
	})
	if err != nil || aws.ToString(translated.TranslatedText) != "redémarrer" {
		t.Fatal(translated, err)
	}

	mlFixture(t, s, "GET", "transcribe/fixtures/restart", "", 200)
	job, err := transcribeClient(
		s,
	).GetTranscriptionJob(ctx, &sdk.GetTranscriptionJobInput{TranscriptionJobName: new("interrupted")})
	if err != nil || aws.ToString(job.TranscriptionJob.FailureReason) != "interrupted by service restart" {
		t.Fatal(job, err)
	}

	batch, err := translateClient(
		s,
	).DescribeTextTranslationJob(ctx, &translate.DescribeTextTranslationJobInput{JobId: new("interrupted")})
	if err != nil || aws.ToString(batch.TextTranslationJobProperties.Message) != "interrupted by service restart" {
		t.Fatal(batch, err)
	}

	mlFixture(t, s, "POST", "reset", "", 200)
	mlFixture(t, s, "GET", "transcribe/fixtures/restart", "", 404)
	mlFixture(t, s, "GET", "translate/fixtures/restart", "", 404)
}

func TestTranscribePaginationAndSharedTags(t *testing.T) {
	t.Parallel()
	s := server(t)
	ctx := context.Background()
	c := transcribeClient(s)
	for _, name := range []string{"z-last", "a-first"} {
		_, err := c.CreateVocabulary(
			ctx,
			&sdk.CreateVocabularyInput{
				VocabularyName: new(name),
				LanguageCode:   types.LanguageCodeEnUs,
				Phrases:        []string{"fixture"},
			},
		)
		if err != nil {
			t.Fatal(err)
		}

		_, err = c.CreateVocabularyFilter(
			ctx,
			&sdk.CreateVocabularyFilterInput{
				VocabularyFilterName: new(name),
				LanguageCode:         types.LanguageCodeEnUs,
				Words:                []string{"fixture"},
			},
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	page, err := c.ListVocabularies(ctx, &sdk.ListVocabulariesInput{MaxResults: new(int32(1))})
	if err != nil || len(page.Vocabularies) != 1 || aws.ToString(page.Vocabularies[0].VocabularyName) != "a-first" ||
		page.NextToken == nil {
		t.Fatal(page, err)
	}

	next, err := c.ListVocabularies(ctx, &sdk.ListVocabulariesInput{NextToken: page.NextToken})
	if err != nil || len(next.Vocabularies) != 1 || aws.ToString(next.Vocabularies[0].VocabularyName) != "z-last" {
		t.Fatal(next, err)
	}

	filters, err := c.ListVocabularyFilters(ctx, &sdk.ListVocabularyFiltersInput{MaxResults: new(int32(1))})
	if err != nil || len(filters.VocabularyFilters) != 1 || filters.NextToken == nil {
		t.Fatal(filters, err)
	}

	filters, err = c.ListVocabularyFilters(ctx, &sdk.ListVocabularyFiltersInput{NextToken: filters.NextToken})
	if err != nil || len(filters.VocabularyFilters) != 1 ||
		aws.ToString(filters.VocabularyFilters[0].VocabularyFilterName) != "z-last" {
		t.Fatal(filters, err)
	}

	_, err = c.ListVocabularyFilters(ctx, &sdk.ListVocabularyFiltersInput{NextToken: new("%%%")})
	wantAccessAnalyzerError(t, err, "BadRequestException")
	tags := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	resource := "arn:aws:transcribe:us-east-1:000000000000:vocabulary/a-first"
	result, err := tags.TagResources(
		ctx,
		&rgta.TagResourcesInput{ResourceARNList: []string{resource}, Tags: map[string]string{"shared": "yes"}},
	)
	if err != nil || len(result.FailedResourcesMap) != 0 {
		t.Fatal(result, err)
	}

	maxTags := make([]types.Tag, 200)
	for i := range maxTags {
		maxTags[i] = types.Tag{Key: new(fmt.Sprintf("key-%03d", i)), Value: new("fixture")}
	}

	tagARN := new("arn:aws:transcribe:us-east-1:000000000000:vocabulary/z-last")
	_, err = c.TagResource(ctx, &sdk.TagResourceInput{ResourceArn: tagARN, Tags: maxTags})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: tagARN, Tags: []types.Tag{{Key: new("overflow"), Value: new("fixture")}}},
	)
	wantAccessAnalyzerError(t, err, "BadRequestException")
}

func TestTranslateAndTranscribeCloudFormation(t *testing.T) {
	t.Parallel()
	s := server(t)
	ctx := context.Background()
	c := cfnClient(s)
	objects := s3Client(s, "test", "us-east-1")
	_, err := objects.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("ml-cfn-input")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = objects.PutObject(
		ctx,
		&s3.PutObjectInput{
			Bucket: new("ml-cfn-input"),
			Key:    new("data.tsv"),
			Body:   strings.NewReader("en\tfr\nhello\tbonjour\n"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	template := func(team, word string) string {
		resources := map[string]any{
			"Terminology": map[string]any{
				"Type": "AWS::Translate::Terminology",
				"Properties": map[string]any{
					"Name":            "cfn-terms",
					"MergeStrategy":   "OVERWRITE",
					"TerminologyData": map[string]any{"File": "en,fr\nhello," + word + "\n", "Format": "CSV"},
					"Tags":            []any{map[string]any{"Key": "team", "Value": team}},
				},
			},
			"Parallel": map[string]any{
				"Type": "AWS::Translate::ParallelData",
				"Properties": map[string]any{
					"Name":               "cfn-parallel",
					"ParallelDataConfig": map[string]any{"S3Uri": "s3://ml-cfn-input/data.tsv", "Format": "TSV"},
					"Tags":               []any{map[string]any{"Key": "team", "Value": team}},
				},
			},
			"Vocabulary": map[string]any{
				"Type": "AWS::Transcribe::Vocabulary",
				"Properties": map[string]any{
					"VocabularyName": "cfn-words",
					"LanguageCode":   "en-US",
					"Phrases":        []string{word},
					"Tags":           []any{map[string]any{"Key": "team", "Value": team}},
				},
			},
			"Filter": map[string]any{
				"Type": "AWS::Transcribe::VocabularyFilter",
				"Properties": map[string]any{
					"VocabularyFilterName": "cfn-filter",
					"LanguageCode":         "en-US",
					"Words":                []string{word},
					"Tags":                 []any{map[string]any{"Key": "team", "Value": team}},
				},
			},
		}
		data, e := json.Marshal(map[string]any{"Resources": resources})
		if e != nil {
			t.Fatal(e)
		}

		return string(data)
	}
	_, err = c.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("ml-fixtures"), TemplateBody: new(template("core", "bonjour"))},
	)
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, c, "ml-fixtures", cfntypes.StackStatusCreateComplete)
	pollML(t, func() bool {
		out, e := transcribeClient(
			s,
		).GetVocabulary(ctx, &sdk.GetVocabularyInput{VocabularyName: new("cfn-words")})
		return e == nil && out.VocabularyState == "READY"
	})
	_, err = c.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("ml-fixtures"), TemplateBody: new(template("platform", "salut"))},
	)
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, c, "ml-fixtures", cfntypes.StackStatusUpdateComplete)
	tags, err := translateClient(
		s,
	).ListTagsForResource(ctx, &translate.ListTagsForResourceInput{
		ResourceArn: new("arn:aws:translate:us-east-1:000000000000:terminology/cfn-terms/LATEST"),
	})
	if err != nil || len(tags.Tags) != 1 || aws.ToString(tags.Tags[0].Value) != "platform" {
		t.Fatal(tags, err)
	}

	_, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("ml-fixtures")})
	if err != nil {
		t.Fatal(err)
	}

	pollML(t, func() bool {
		_, e := c.DescribeStacks(ctx, &cfn.DescribeStacksInput{StackName: new("ml-fixtures")})
		return e != nil
	})
}

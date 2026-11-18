package integration_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/comprehend"
	comprehendtypes "github.com/aws/aws-sdk-go-v2/service/comprehend/types"
	"github.com/aws/aws-sdk-go-v2/service/polly"
	pollytypes "github.com/aws/aws-sdk-go-v2/service/polly/types"
	"github.com/aws/aws-sdk-go-v2/service/rekognition"
	rekognitiontypes "github.com/aws/aws-sdk-go-v2/service/rekognition/types"
	"github.com/aws/aws-sdk-go-v2/service/textract"
	textracttypes "github.com/aws/aws-sdk-go-v2/service/textract/types"
)

// TestAIServicesSDK drives each fixture-backed AI service through its official SDK v2 client, so the wire formats
// match what the SDKs send and parse.
func TestAIServicesSDK(t *testing.T) {
	t.Parallel()
	s := server(t)
	region, endpoint, credentials := "us-east-1", new(s.URL), creds("test")

	t.Run("Comprehend", func(t *testing.T) {
		c := comprehend.New(comprehend.Options{
			Region: region, BaseEndpoint: endpoint, Credentials: credentials, Retryer: aws.NopRetryer{},
		})
		const text = "TinyStack answers from fixtures."
		_, err := c.DetectSentiment(context.Background(), &comprehend.DetectSentimentInput{
			Text: new(text), LanguageCode: comprehendtypes.LanguageCodeEn,
		})
		if _, ok := errors.AsType[*comprehendtypes.InvalidRequestException](err); !ok {
			t.Fatalf("without a fixture: %v", err)
		}

		registerAI(t, s, "comprehend", "sdk", "DetectSentiment", fmt.Sprintf(`{"text":%q,"languageCode":"en"}`, text),
			`{"Sentiment":"POSITIVE","SentimentScore":{"Positive":0.99,"Negative":0,"Neutral":0.01,"Mixed":0}}`)
		out, err := c.DetectSentiment(context.Background(), &comprehend.DetectSentimentInput{
			Text: new(text), LanguageCode: comprehendtypes.LanguageCodeEn,
		})
		if err != nil || out.Sentiment != comprehendtypes.SentimentTypePositive ||
			*out.SentimentScore.Positive != 0.99 {
			t.Fatal(out, err)
		}
	})

	t.Run("Textract", func(t *testing.T) {
		c := textract.New(textract.Options{
			Region: region, BaseEndpoint: endpoint, Credentials: credentials, Retryer: aws.NopRetryer{},
		})
		doc := aiPNG(t, 7)
		registerAI(t, s, "textract", "sdk", "DetectDocumentText", fmt.Sprintf(`{"sha256":%q}`, aiDigest(doc)),
			`{"DocumentMetadata":{"Pages":1},"Blocks":[{"BlockType":"LINE","Text":"from the SDK"}]}`)
		out, err := c.DetectDocumentText(context.Background(), &textract.DetectDocumentTextInput{
			Document: &textracttypes.Document{Bytes: doc},
		})
		if err != nil || len(out.Blocks) != 1 || out.Blocks[0].BlockType != textracttypes.BlockTypeLine ||
			*out.Blocks[0].Text != "from the SDK" {
			t.Fatal(out, err)
		}
	})

	t.Run("Rekognition", func(t *testing.T) {
		c := rekognition.New(rekognition.Options{
			Region: region, BaseEndpoint: endpoint, Credentials: credentials, Retryer: aws.NopRetryer{},
		})
		img := aiPNG(t, 8)
		registerAI(t, s, "rekognition", "sdk", "DetectLabels", fmt.Sprintf(`{"sha256":%q}`, aiDigest(img)),
			`{"Labels":[{"Name":"Cat","Confidence":98.5}]}`)
		labels, err := c.DetectLabels(
			context.Background(),
			&rekognition.DetectLabelsInput{Image: &rekognitiontypes.Image{Bytes: img}},
		)
		if err != nil || len(labels.Labels) != 1 || *labels.Labels[0].Name != "Cat" {
			t.Fatal(labels, err)
		}

		created, err := c.CreateCollection(context.Background(), &rekognition.CreateCollectionInput{
			CollectionId: new("sdk-faces"), Tags: map[string]string{"team": "ml"},
		})
		if err != nil || created.CollectionArn == nil {
			t.Fatal(created, err)
		}

		listed, err := c.ListCollections(context.Background(), &rekognition.ListCollectionsInput{})
		if err != nil || len(listed.CollectionIds) != 1 || listed.CollectionIds[0] != "sdk-faces" {
			t.Fatal(listed, err)
		}
	})

	t.Run("Polly", func(t *testing.T) {
		c := polly.New(polly.Options{
			Region: region, BaseEndpoint: endpoint, Credentials: credentials, Retryer: aws.NopRetryer{},
		})
		voices, err := c.DescribeVoices(
			context.Background(),
			&polly.DescribeVoicesInput{LanguageCode: pollytypes.LanguageCodeEnUs},
		)
		if err != nil || len(voices.Voices) == 0 {
			t.Fatal(voices, err)
		}

		speech, err := c.SynthesizeSpeech(context.Background(), &polly.SynthesizeSpeechInput{
			Text: new("hello"), VoiceId: pollytypes.VoiceIdJoanna, OutputFormat: pollytypes.OutputFormatPcm,
		})
		if err != nil {
			t.Fatal(err)
		}

		audio, err := io.ReadAll(speech.AudioStream)
		_ = speech.AudioStream.Close()
		if err != nil || *speech.ContentType != "audio/pcm" || speech.RequestCharacters != 5 ||
			!bytes.Equal(audio, make([]byte, 3200)) {
			t.Fatal(speech.ContentType, speech.RequestCharacters, len(audio), err)
		}

		const bucket = "polly-sdk"
		aiBucket(t, s, bucket)
		task, err := c.StartSpeechSynthesisTask(context.Background(), &polly.StartSpeechSynthesisTaskInput{
			Text: new("hello"), VoiceId: pollytypes.VoiceIdJoanna, OutputFormat: pollytypes.OutputFormatMp3,
			OutputS3BucketName: new(bucket),
		})
		if err != nil {
			t.Fatal(err)
		}

		deadline := time.Now().Add(10 * time.Second)
		for {
			got, getErr := c.GetSpeechSynthesisTask(context.Background(), &polly.GetSpeechSynthesisTaskInput{
				TaskId: task.SynthesisTask.TaskId,
			})
			if getErr != nil {
				t.Fatal(getErr)
			}

			if got.SynthesisTask.TaskStatus == pollytypes.TaskStatusCompleted {
				break
			}

			if got.SynthesisTask.TaskStatus == pollytypes.TaskStatusFailed || time.Now().After(deadline) {
				t.Fatalf("task = %+v", got.SynthesisTask)
			}

			time.Sleep(50 * time.Millisecond)
		}
	})
}

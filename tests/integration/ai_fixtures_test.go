package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// aiRequest uses the SDK v2 signer with the pinned wire protocols.
func aiRequest(
	t *testing.T, s *httptest.Server, service, key, region, method, path, target, body string,
) (status int, headers http.Header, data []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, s.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Content-Type", "application/json")
	if target != "" {
		req.Header.Set("X-Amz-Target", target)
		req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	}

	hash := sha256.Sum256([]byte(body))
	err = v4.NewSigner().SignHTTP(
		ctx, aws.Credentials{AccessKeyID: key, SecretAccessKey: "test"},
		req, hex.EncodeToString(hash[:]), service, region, time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}

	response, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer response.Body.Close()
	data, err = io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}

	return response.StatusCode, response.Header, data
}

func aiRPC(t *testing.T, s *httptest.Server, service, target, body string, status int, code string) []byte {
	t.Helper()
	got, header, data := aiRequest(t, s, service, "test", "us-east-1", http.MethodPost, "/", target, body)
	assertAIResponse(t, got, header, data, status, code)
	return data
}

func aiPolly(
	t *testing.T, s *httptest.Server, method, path, body string, status int, code string,
) (header http.Header, data []byte) {
	t.Helper()
	got, header, data := aiRequest(t, s, "polly", "test", "us-east-1", method, path, "", body)
	assertAIResponse(t, got, header, data, status, code)
	return header, data
}

func assertAIResponse(t *testing.T, got int, header http.Header, data []byte, status int, code string) {
	t.Helper()
	if got != status || (code != "" && !strings.Contains(header.Get("X-Amzn-ErrorType"), code) &&
		!bytes.Contains(data, []byte(code))) {
		t.Fatalf("response = %d %s %s, want %d %s", got, header.Get("X-Amzn-ErrorType"), data, status, code)
	}
}

func aiJSON[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}

	return out
}

func registerAI(t *testing.T, s *httptest.Server, service, id, op, input, output string) {
	t.Helper()
	body := fmt.Sprintf(`{"operation":%q,"input":%s,"output":%s}`, op, input, output)
	mlFixture(t, s, http.MethodPut, service+"/fixtures/"+id, body, http.StatusOK)
}

func TestComprehendFixtures(t *testing.T) {
	t.Parallel()
	s := server(t)
	const (
		service = "comprehend"
		prefix  = "Comprehend_20171127."
		text    = "The local fixture is deterministic."
	)
	for _, tc := range []struct{ op, language, output string }{
		{"DetectSentiment", "en", `{"Sentiment":"POSITIVE","SentimentScore":{"Positive":0.99}}`},
		{"DetectEntities", "en", `{"Entities":[{"Type":"OTHER","Text":"fixture","Score":0.99}]}`},
		{"DetectKeyPhrases", "en", `{"KeyPhrases":[{"Text":"local fixture","Score":0.98}]}`},
		{"DetectDominantLanguage", "", `{"Languages":[{"LanguageCode":"en","Score":0.97}]}`},
		{"DetectPiiEntities", "en", `{"Entities":[{"Type":"NAME","Score":0.96,"BeginOffset":0,"EndOffset":3}]}`},
		{"DetectSyntax", "en", `{"SyntaxTokens":[{"Text":"The","PartOfSpeech":{"Tag":"DET"}}]}`},
		{"ContainsPiiEntities", "en", `{"Labels":[{"Name":"NAME","Score":0.95}]}`},
	} {
		t.Run(tc.op, func(t *testing.T) {
			fixture, request := fmt.Sprintf(`{"text":%q}`, text), fmt.Sprintf(`{"Text":%q}`, text)
			if tc.language != "" {
				fixture = fmt.Sprintf(`{"text":%q,"languageCode":%q}`, text, tc.language)
				request = fmt.Sprintf(`{"Text":%q,"LanguageCode":%q}`, text, tc.language)
			}

			data := aiRPC(t, s, service, prefix+tc.op, request, 400, "InvalidRequestException")
			if !bytes.Contains(data, []byte("/_tinystack/comprehend/fixtures")) {
				t.Fatalf("missing fixture response = %s", data)
			}

			registerAI(t, s, service, tc.op, tc.op, fixture, tc.output)
			data = aiRPC(t, s, service, prefix+tc.op, request, 200, "")
			if !reflect.DeepEqual(aiJSON[any](t, data), aiJSON[any](t, []byte(tc.output))) {
				t.Fatalf("fixture = %s, want %s", data, tc.output)
			}
		})
	}

	body := fmt.Sprintf(`{"Text":%q,"LanguageCode":"en"}`, text)
	status, header, data := aiRequest(t, s, service, "test", "eu-west-1", "POST", "/", prefix+"DetectSentiment", body)
	assertAIResponse(t, status, header, data, 200, "")
	status, header, data = aiRequest(t, s, service, "team-b", "us-east-1", "POST", "/", prefix+"DetectSentiment", body)
	assertAIResponse(t, status, header, data, 400, "InvalidRequestException")
	registerAI(t, s, service, "duplicate", "DetectSentiment", fmt.Sprintf(`{"text":%q,"languageCode":"en"}`, text),
		`{"Sentiment":"NEGATIVE"}`)
	aiRPC(t, s, service, prefix+"DetectSentiment", body, 400, "InvalidRequestException")
	aiRPC(t, s, service, prefix+"DetectPiiEntities", `{"Text":"valid","LanguageCode":"fr"}`, 400,
		"UnsupportedLanguageException")
	tooLong := fmt.Sprintf(`{"Text":%q,"LanguageCode":"en"}`, strings.Repeat("é", 2501))
	aiRPC(t, s, service, prefix+"DetectSentiment", tooLong, 400, "TextSizeLimitExceededException")
	aiRPC(t, s, service, prefix+"BatchDetectEntities", `{}`, 400, "UnknownOperationException")
}

func aiPNG(t *testing.T, tint uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 100, 100))
	img.SetRGBA(0, 0, color.RGBA{R: tint, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

func aiDigest(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func aiBucket(t *testing.T, s *httptest.Server, bucket string) *s3.Client {
	t.Helper()
	c := s3Client(s, "test", "us-east-1")
	_, err := c.CreateBucket(context.Background(), &s3.CreateBucketInput{Bucket: new(bucket)})
	if err != nil {
		t.Fatal(err)
	}

	return c
}

func aiPutObject(t *testing.T, c *s3.Client, bucket, key string, data []byte) {
	t.Helper()
	_, err := c.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: new(bucket), Key: new(key),
		Body: bytes.NewReader(data),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTextractFixtures(t *testing.T) {
	t.Parallel()
	s := server(t)
	const bucket = "textract-fixture"
	c := aiBucket(t, s, bucket)
	doc := aiPNG(t, 1)
	aiPutObject(t, c, bucket, "page.png", doc)
	inline := fmt.Sprintf(`{"Document":{"Bytes":%q}}`, base64.StdEncoding.EncodeToString(doc))
	ref := `{"Document":{"S3Object":{"Bucket":"textract-fixture","Name":"page.png"}}}`
	key := fmt.Sprintf(`{"sha256":%q}`, aiDigest(doc))
	const output = `{"DocumentMetadata":{"Pages":1},"Blocks":[{"BlockType":"LINE","Text":"fixture"}]}`
	aiRPC(t, s, "textract", "Textract.DetectDocumentText", inline, 400, "InvalidParameterException")
	registerAI(t, s, "textract", "page", "DetectDocumentText", key, output)
	for _, request := range []string{inline, ref} {
		data := aiRPC(t, s, "textract", "Textract.DetectDocumentText", request, 200, "")
		if !reflect.DeepEqual(aiJSON[any](t, data), aiJSON[any](t, []byte(output))) {
			t.Fatalf("document result = %s", data)
		}
	}

	registerAI(t, s, "textract", "forms", "AnalyzeDocument",
		fmt.Sprintf(`{"sha256":%q,"featureTypes":["TABLES","FORMS"]}`, aiDigest(doc)),
		`{"Blocks":[{"BlockType":"KEY_VALUE_SET","EntityTypes":["KEY"]}]}`)
	analyze := strings.TrimSuffix(ref, "}") + `,"FeatureTypes":["FORMS","TABLES"]}`
	data := aiRPC(t, s, "textract", "Textract.AnalyzeDocument", analyze, 200, "")
	if !bytes.Contains(data, []byte("KEY_VALUE_SET")) {
		t.Fatalf("analysis result = %s", data)
	}

	aiRPC(t, s, "textract", "Textract.DetectDocumentText",
		strings.Replace(ref, "page.png", "missing", 1), 400, "InvalidS3ObjectException")
	aiRPC(t, s, "textract", "Textract.DetectDocumentText",
		`{"Document":{"Bytes":"aW52YWxpZA=="}}`, 400, "UnsupportedDocumentException")
	aiRPC(t, s, "textract", "Textract.AnalyzeDocument",
		strings.Replace(analyze, `"FORMS","TABLES"`, `"QUERIES"`, 1), 400, "InvalidParameterException")
	aiRPC(t, s, "textract", "Textract.AnalyzeDocument",
		strings.TrimSuffix(analyze, "}")+`,"AdaptersConfig":{}}`, 400, "ValidationException")
}

func TestRekognitionFixtures(t *testing.T) {
	t.Parallel()
	s := server(t)
	const service = "rekognition"
	const prefix = "RekognitionService."
	const bucket = "rekognition-fixture"
	c := aiBucket(t, s, bucket)
	img, target := aiPNG(t, 2), aiPNG(t, 3)
	aiPutObject(t, c, bucket, "target.png", target)
	imageInput := fmt.Sprintf(`{"Bytes":%q}`, base64.StdEncoding.EncodeToString(img))
	request := fmt.Sprintf(`{"Image":%s}`, imageInput)
	key := fmt.Sprintf(`{"sha256":%q}`, aiDigest(img))
	aiRPC(t, s, service, prefix+"DetectLabels", request, 400, "InvalidParameterException")
	for _, tc := range []struct{ op, output, want string }{
		{"DetectLabels", `{"Labels":[{"Name":"Low","Confidence":70},{"Name":"High","Confidence":97}]}`, "High"},
		{"DetectFaces", `{"FaceDetails":[{"Confidence":92}]}`, "FaceDetails"},
		{"DetectText", `{"TextDetections":[{"DetectedText":"readable","Type":"LINE"}]}`, "readable"},
		{"DetectModerationLabels", `{"ModerationLabels":[{"Name":"Fixture","Confidence":91}]}`, "Fixture"},
	} {
		registerAI(t, s, service, tc.op, tc.op, key, tc.output)
		body := request
		if tc.op == "DetectLabels" {
			body = strings.TrimSuffix(request, "}") + `,"MaxLabels":1,"MinConfidence":60}`
		}

		data := aiRPC(t, s, service, prefix+tc.op, body, 200, "")
		if !bytes.Contains(data, []byte(tc.want)) || bytes.Contains(data, []byte("Low")) {
			t.Fatalf("%s = %s", tc.op, data)
		}
	}

	registerAI(t, s, service, "compare", "CompareFaces",
		fmt.Sprintf(`{"sha256":%q,"targetSha256":%q}`, aiDigest(img), aiDigest(target)),
		`{"FaceMatches":[{"Similarity":86,"Face":{"Confidence":96}},{"Similarity":95,"Face":{"Confidence":98}}]}`)
	compare := fmt.Sprintf(
		`{"SourceImage":%s,"TargetImage":{"S3Object":{"Bucket":%q,"Name":"target.png"}},"SimilarityThreshold":90}`,
		imageInput, bucket,
	)
	result := aiJSON[struct {
		FaceMatches    []map[string]any
		UnmatchedFaces []map[string]any
	}](t, aiRPC(t, s, service, prefix+"CompareFaces", compare, 200, ""))
	if len(result.FaceMatches) != 1 || result.FaceMatches[0]["Similarity"] != float64(95) ||
		len(result.UnmatchedFaces) != 1 {
		t.Fatalf("compare = %+v", result)
	}

	aiRPC(t, s, service, prefix+"DetectFaces", `{"Image":{}}`, 400, "InvalidParameterException")
	aiRPC(t, s, service, prefix+"DetectFaces",
		strings.TrimSuffix(request, "}")+`,"Attributes":["ALL"]}`, 400, "ValidationException")
}

func TestRekognitionCollections(t *testing.T) {
	t.Parallel()
	s := server(t)
	const prefix = "RekognitionService."
	rpc := func(op, body string, status int, code string) []byte {
		return aiRPC(t, s, "rekognition", prefix+op, body, status, code)
	}
	rpc("CreateCollection", `{"CollectionId":"faces-a","Tags":{"initial":"yes"}}`, 200, "")
	row := aiJSON[struct{ CollectionArn string }](t, rpc("CreateCollection", `{"CollectionId":"faces-b"}`, 200, ""))
	rpc("CreateCollection", `{"CollectionId":"faces-b"}`, 400, "ResourceAlreadyExistsException")
	page := aiJSON[struct {
		NextToken     string
		CollectionIds []string
	}](t, rpc("ListCollections", `{"MaxResults":1}`, 200, ""))
	if !reflect.DeepEqual(page.CollectionIds, []string{"faces-a"}) || page.NextToken == "" {
		t.Fatalf("page = %+v", page)
	}

	data := rpc("ListCollections", fmt.Sprintf(`{"NextToken":%q}`, page.NextToken), 200, "")
	if !bytes.Contains(data, []byte("faces-b")) {
		t.Fatalf("next page = %s", data)
	}

	tagging := rgta.New(rgta.Options{
		Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test"), Retryer: aws.NopRetryer{},
	})
	tagged, err := tagging.TagResources(context.Background(), &rgta.TagResourcesInput{
		ResourceARNList: []string{row.CollectionArn}, Tags: map[string]string{"owner": "team"},
	})
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatalf("tag resources = %+v, %v", tagged, err)
	}

	arn := fmt.Sprintf(`{"ResourceArn":%q}`, row.CollectionArn)
	data = rpc("ListTagsForResource", arn, 200, "")
	if !bytes.Contains(data, []byte(`"owner":"team"`)) {
		t.Fatalf("tags = %s", data)
	}

	rpc("UntagResource", strings.TrimSuffix(arn, "}")+`,"TagKeys":["owner"]}`, 200, "")
	data = rpc("ListTagsForResource", arn, 200, "")
	if bytes.Contains(data, []byte("owner")) {
		t.Fatalf("removed tag remains: %s", data)
	}

	rpc("DescribeCollection", `{"CollectionId":"faces-b"}`, 200, "")
	rpc("DeleteCollection", `{"CollectionId":"faces-b"}`, 200, "")
	rpc("DescribeCollection", `{"CollectionId":"faces-b"}`, 400, "ResourceNotFoundException")
}

func TestPollySpeechAndTasks(t *testing.T) {
	t.Parallel()
	s := server(t)
	const (
		bucket     = "polly-fixture"
		speechPath = "/v1/speech"
		tasksPath  = "/v1/synthesisTasks"
		pcmBody    = `{"Text":"hello","VoiceId":"Joanna","OutputFormat":"pcm"}`
	)
	c := aiBucket(t, s, bucket)
	_, voices := aiPolly(t, s, "GET", "/v1/voices?LanguageCode=en-US", "", 200, "")
	if !bytes.Contains(voices, []byte("Joanna")) || bytes.Contains(voices, []byte("Amy")) {
		t.Fatalf("voices = %s", voices)
	}

	headers, data := aiPolly(t, s, "POST", speechPath, pcmBody, 200, "")
	if headers.Get("Content-Type") != "audio/pcm" || headers.Get("X-Amzn-RequestCharacters") != "5" ||
		!bytes.Equal(data, make([]byte, 3200)) {
		t.Fatalf("PCM headers = %v, length = %d", headers, len(data))
	}

	for _, tc := range []struct {
		format, contentType string
		magic               []byte
	}{
		{"mp3", "audio/mpeg", []byte{0xff}},
		{"ogg_vorbis", "audio/ogg", []byte("OggS")},
	} {
		body := fmt.Sprintf(`{"Text":"hello","VoiceId":"Amy","OutputFormat":%q,"SampleRate":"24000"}`, tc.format)
		audioHeaders, audio := aiPolly(t, s, "POST", speechPath, body, 200, "")
		if audioHeaders.Get("Content-Type") != tc.contentType || !bytes.HasPrefix(audio, tc.magic) {
			t.Fatalf("%s headers = %v, length = %d", tc.format, audioHeaders, len(audio))
		}
	}

	headers, data = aiPolly(t, s, "POST", speechPath,
		`{"Text":"é world","VoiceId":"Joanna","OutputFormat":"json","SpeechMarkTypes":["word"]}`, 200, "")
	if headers.Get("Content-Type") != "application/x-json-stream" ||
		!bytes.Contains(data, []byte(`"value":"world","time":0,"start":3,"end":8`)) {
		t.Fatalf("marks = %v %s", headers, data)
	}

	registerAI(t, s, "polly", "speech", "SynthesizeSpeech",
		`{"text":"hello","voiceId":"Joanna","outputFormat":"pcm"}`, `{"AudioStream":"AQIDBA=="}`)
	_, data = aiPolly(t, s, "POST", speechPath, pcmBody, 200, "")
	if !bytes.Equal(data, []byte{1, 2, 3, 4}) {
		t.Fatalf("fixture bytes = %v", data)
	}

	aiPolly(t, s, "POST", speechPath, strings.TrimSuffix(pcmBody, "}")+`,"SampleRate":"22050"}`,
		400, "InvalidSampleRateException")
	aiPolly(t, s, "POST", speechPath, strings.TrimSuffix(pcmBody, "}")+`,"LexiconNames":["local"]}`,
		501, "NotImplemented")
	body := strings.TrimSuffix(pcmBody, "}") + `,"OutputS3BucketName":"polly-fixture","OutputS3KeyPrefix":"tasks/"}`
	_, data = aiPolly(t, s, "POST", tasksPath, body, 200, "")
	type task struct{ TaskId, TaskStatus, TaskStatusReason, OutputUri string }
	started := aiJSON[struct{ SynthesisTask task }](t, data).SynthesisTask
	if started.TaskId == "" || started.TaskStatus != "scheduled" {
		t.Fatalf("start task = %+v", started)
	}

	var done task
	pollML(t, func() bool {
		_, taskBody := aiPolly(t, s, "GET", tasksPath+"/"+started.TaskId, "", 200, "")
		done = aiJSON[struct{ SynthesisTask task }](t, taskBody).SynthesisTask
		if done.TaskStatus == "failed" {
			t.Fatalf("task failed: %s", done.TaskStatusReason)
		}

		return done.TaskStatus == "completed"
	})
	uri, err := url.Parse(done.OutputUri)
	if err != nil || uri.Scheme != "s3" || uri.Host != bucket || !strings.HasPrefix(uri.Path, "/tasks/") {
		t.Fatalf("output URI = %q, %v", done.OutputUri, err)
	}

	obj, err := c.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: new(uri.Host), Key: new(strings.TrimPrefix(uri.Path, "/")),
	})
	if err != nil {
		t.Fatal(err)
	}

	defer obj.Body.Close()
	data, err = io.ReadAll(obj.Body)
	if err != nil || !bytes.Equal(data, []byte{1, 2, 3, 4}) || aws.ToString(obj.ContentType) != "audio/pcm" {
		t.Fatalf("task audio = %v %v", data, err)
	}

	_, data = aiPolly(t, s, "GET", tasksPath+"?Status=completed&MaxResults=1", "", 200, "")
	if !bytes.Contains(data, []byte(started.TaskId)) {
		t.Fatalf("task list = %s", data)
	}

	aiPolly(t, s, "POST", tasksPath, strings.Replace(body, bucket, "not-created", 1), 400, "InvalidS3BucketException")
}

func TestPollyLexicons(t *testing.T) {
	t.Parallel()
	s := server(t)
	const path = "/v1/lexicons/Local"
	const pls = `<lexicon xmlns="http://www.w3.org/2005/01/pronunciation-lexicon" xml:lang="en-US" alphabet="ipa">` +
		`<lexeme><grapheme>fixture</grapheme><alias>example</alias></lexeme></lexicon>`
	aiPolly(t, s, "PUT", path, `{"Content":"invalid"}`, 400, "InvalidLexiconException")
	unknownLanguage := strings.Replace(pls, "en-US", "xx-XX", 1)
	aiPolly(t, s, "PUT", path, fmt.Sprintf(`{"Content":%q}`, unknownLanguage), 400, "UnsupportedPlsLanguageException")
	aiPolly(t, s, "PUT", path, fmt.Sprintf(`{"Content":%q}`, pls), 200, "")
	_, data := aiPolly(t, s, "GET", path, "", 200, "")
	result := aiJSON[struct {
		Lexicon           struct{ Content, Name string }
		LexiconAttributes struct{ LexemesCount int }
	}](t, data)
	if result.Lexicon.Content != pls || result.Lexicon.Name != "Local" || result.LexiconAttributes.LexemesCount != 1 {
		t.Fatalf("lexicon = %s", data)
	}

	_, data = aiPolly(t, s, "GET", "/v1/lexicons", "", 200, "")
	if !bytes.Contains(data, []byte(`"Name":"Local"`)) {
		t.Fatalf("lexicon list = %s", data)
	}

	aiPolly(t, s, "DELETE", path, "", 200, "")
	aiPolly(t, s, "GET", path, "", 404, "LexiconNotFoundException")
}

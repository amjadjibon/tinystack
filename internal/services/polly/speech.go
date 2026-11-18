package polly

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/polly"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/mlfixtures"
)

const (
	pcm            = "pcm"
	mp3            = "mp3"
	oggVorbis      = "ogg_vorbis"
	jsonFormat     = "json"
	plainText      = "text"
	syncCharacters = 3000
	taskCharacters = 100000
	defaultRate    = "22050"
)

// The bundled files contain mono silence encoded at each supported sample rate.
//
//go:embed silence/*.mp3 silence/*.ogg
var silence embed.FS

type speechInput struct {
	Text            string   `json:"text"`
	VoiceID         string   `json:"voiceId"`
	Engine          string   `json:"engine,omitempty"`
	LanguageCode    string   `json:"languageCode,omitempty"`
	OutputFormat    string   `json:"outputFormat"`
	SampleRate      string   `json:"sampleRate,omitempty"`
	TextType        string   `json:"textType,omitempty"`
	SpeechMarkTypes []string `json:"speechMarkTypes,omitempty"`
}

type speechOutput struct {
	AudioStream *[]byte `json:"AudioStream"`
}

func (in *speechInput) normalize(maxCharacters int) error {
	if !utf8.ValidString(in.Text) || in.Text == "" {
		return awserror.New("InvalidParameterValueException", "Text must be nonempty UTF-8")
	}

	if utf8.RuneCountInString(in.Text) > maxCharacters {
		return awserror.New("TextLengthExceededException", "Text exceeds the supported character limit")
	}

	if in.Engine == "" {
		in.Engine = standard
	}

	if in.Engine != standard {
		return awserror.New("EngineNotSupportedException", "only the standard engine is supported")
	}

	var language string
	for _, v := range voices {
		if v.id == in.VoiceID {
			language = v.language
			break
		}
	}

	if language == "" || (in.LanguageCode != "" && in.LanguageCode != language) {
		return awserror.New("LanguageNotSupportedException", "VoiceId and LanguageCode must match a local voice")
	}

	in.LanguageCode = language
	if in.TextType == "" {
		in.TextType = plainText
	}

	if in.TextType != plainText {
		return awserror.New("InvalidSsmlException", "only plain text is supported")
	}

	return in.normalizeFormat()
}

func (in *speechInput) normalizeFormat() error {
	if in.OutputFormat != jsonFormat && len(in.SpeechMarkTypes) != 0 {
		return awserror.New("MarksNotSupportedForFormatException", "SpeechMarkTypes requires OutputFormat=json")
	}

	switch in.OutputFormat {
	case mp3, oggVorbis:
		switch in.SampleRate {
		case "":
			in.SampleRate = defaultRate
		case "8000", "16000", defaultRate, "24000", "44100", "48000":
		default:
			return awserror.New("InvalidSampleRateException", "invalid SampleRate for compressed audio")
		}
	case pcm:
		switch in.SampleRate {
		case "":
			in.SampleRate = "16000"
		case "8000", "16000":
		default:
			return awserror.New("InvalidSampleRateException", "PCM SampleRate must be 8000 or 16000")
		}
	case jsonFormat:
		if in.SampleRate != "" {
			return awserror.New("InvalidSampleRateException", "SampleRate is unsupported for JSON speech marks")
		}

		marks := slices.Clone(in.SpeechMarkTypes)
		slices.Sort(marks)
		if len(marks) == 0 || len(marks) != len(slices.Compact(marks)) {
			return awserror.New("MarksNotSupportedForFormatException", "provide unique SpeechMarkTypes for JSON")
		}

		for _, kind := range marks {
			switch kind {
			case "sentence", "word":
			case "ssml":
				return awserror.New("SsmlMarksNotSupportedForTextTypeException", "SSML marks require SSML text")
			default:
				return awserror.New("MarksNotSupportedForFormatException", "only sentence and word marks are supported")
			}
		}

		in.SpeechMarkTypes = marks
	default:
		return awserror.New(
			"InvalidParameterValueException",
			"supported OutputFormat values: mp3, ogg_vorbis, pcm, json",
		)
	}

	return nil
}

func validateFixture(f *mlfixtures.Reply[*speechInput]) error {
	if f.Operation != synthesize {
		return errors.New("unsupported Polly fixture operation")
	}

	in := f.Input
	if in == nil {
		return errors.New("input is required")
	}

	if err := in.normalize(taskCharacters); err != nil {
		return err
	}

	out, err := mlfixtures.Decode[speechOutput](f.Output)
	if err != nil {
		return err
	}

	if out.AudioStream == nil {
		return errors.New("output.AudioStream must be a base64 string")
	}

	if in.OutputFormat != jsonFormat && len(*out.AudioStream) == 0 {
		return errors.New("output.AudioStream must contain audio bytes")
	}

	if in.OutputFormat == pcm && len(*out.AudioStream)%2 != 0 {
		return errors.New("PCM output requires complete 16-bit samples")
	}

	if in.OutputFormat == jsonFormat {
		for line := range bytes.Lines(*out.AudioStream) {
			line = bytes.TrimSpace(line)
			if !json.Valid(line) || line[0] != '{' {
				return errors.New("JSON speech marks must contain one JSON object per line")
			}
		}
	}

	return nil
}

func (in *speechInput) matches(key *speechInput) bool {
	return key != nil && key.Text == in.Text && key.VoiceID == in.VoiceID && key.Engine == in.Engine &&
		key.LanguageCode == in.LanguageCode && key.OutputFormat == in.OutputFormat && key.SampleRate == in.SampleRate &&
		key.TextType == in.TextType && slices.Equal(key.SpeechMarkTypes, in.SpeechMarkTypes)
}

func (s *Service) audio(ctx context.Context, in *speechInput) (data []byte, contentType string, err error) {
	contentTypes := map[string]string{
		mp3: "audio/mpeg", oggVorbis: "audio/ogg", pcm: "audio/pcm", jsonFormat: "application/x-json-stream",
	}
	contentType = contentTypes[in.OutputFormat]
	raw, err := mlfixtures.Find(ctx, s.store, serviceName, synthesize, in.matches)
	if errors.Is(err, mlfixtures.ErrAmbiguous) {
		return nil, "", awserror.New("ServiceFailureException", "multiple matching Polly fixtures")
	}

	if err != nil {
		return nil, "", err
	}

	if raw != nil {
		out, decodeErr := mlfixtures.Decode[speechOutput](raw)
		if decodeErr != nil {
			return nil, "", decodeErr
		}

		return *out.AudioStream, contentType, nil
	}

	switch in.OutputFormat {
	case pcm:
		rate, parseErr := strconv.Atoi(in.SampleRate)
		if parseErr != nil {
			return nil, "", parseErr
		}

		return make([]byte, rate/10*2), contentType, nil
	case jsonFormat:
		data, err = speechMarks(in)
		return data, contentType, err
	default:
		extension := in.OutputFormat
		if extension == oggVorbis {
			extension = oggExt
		}

		data, err = silence.ReadFile("silence/" + in.SampleRate + "." + extension)
		return data, contentType, err
	}
}

// Offsets are UTF-8 byte offsets. All placeholder times are zero.
type mark struct {
	Type  string `json:"type"`
	Value string `json:"value"`
	Time  int    `json:"time"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

var words = regexp.MustCompile(`[\pL\pM\pN]+(?:['’][\pL\pM\pN]+)*`)

func speechMarks(in *speechInput) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	if slices.Contains(in.SpeechMarkTypes, "sentence") {
		if err := encoder.Encode(mark{Type: "sentence", End: len(in.Text), Value: in.Text}); err != nil {
			return nil, err
		}
	}

	if slices.Contains(in.SpeechMarkTypes, "word") {
		for _, span := range words.FindAllStringIndex(in.Text, -1) {
			start, end := span[0], span[1]
			if err := encoder.Encode(
				mark{Type: "word", Start: start, End: end, Value: in.Text[start:end]},
			); err != nil {
				return nil, err
			}
		}
	}

	return buf.Bytes(), nil
}

func (s *Service) SynthesizeSpeech(
	ctx context.Context,
	in *api.SynthesizeSpeechInput,
) (*api.SynthesizeSpeechOutput, error) {
	key := &speechInput{
		Text: common.Value(in.Text), VoiceID: common.Value(in.VoiceId),
		Engine: common.Value(in.Engine), LanguageCode: common.Value(in.LanguageCode),
		OutputFormat: common.Value(in.OutputFormat), SampleRate: common.Value(in.SampleRate),
		SpeechMarkTypes: in.SpeechMarkTypes, TextType: common.Value(in.TextType),
	}
	if err := key.normalize(syncCharacters); err != nil {
		return nil, err
	}

	data, contentType, err := s.audio(ctx, key)
	if err != nil {
		return nil, err
	}

	return &api.SynthesizeSpeechOutput{
		AudioStream:       data,
		ContentType:       new(contentType),
		RequestCharacters: new(int64(utf8.RuneCountInString(key.Text))),
	}, nil
}

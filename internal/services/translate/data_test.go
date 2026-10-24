package translate

import (
	"context"
	"strings"
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/translate"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestParseData(t *testing.T) {
	for _, tc := range []struct {
		name, format, data string
		valid              bool
	}{
		{"CSV", "CSV", "en,fr\nhello,bonjour\n", true},
		{"TSV", "TSV", "en\tfr\nhello\tbonjour\n", true},
		{"TMX", "TMX", `<tmx><body><tu><tuv xml:lang="en"><seg>hello</seg></tuv>` +
			`<tuv xml:lang="fr"><seg>bonjour</seg></tuv></tu></body></tmx>`, true},
		{"unequal", "CSV", "en,fr\nhello\n", false},
		{"empty", "CSV", "en,fr\n", false},
		{"bad XML", "TMX", `<tmx><body>`, false},
		{"trailing XML", "TMX", `<tmx><body><tu><tuv xml:lang="en"><seg>a</seg></tuv>` +
			`<tuv xml:lang="fr"><seg>b</seg></tuv></tu></body></tmx><extra/>`, false},
		{"term bytes", "CSV", "en,fr\n" + strings.Repeat("a", 201) + ",bonjour\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			langs, count, err := parseData([]byte(tc.data), tc.format, true)
			if (err == nil) != tc.valid {
				t.Fatalf("languages=%v count=%d err=%v", langs, count, err)
			}

			if tc.valid && (len(langs) != 2 || langs[0] != "en" || langs[1] != "fr" || count != 1) {
				t.Fatal(langs, count)
			}
		})
	}
}

func TestGeneratedValidation(t *testing.T) {
	s := New(memory.New(nil, nil), nil, nil, nil)
	defer s.Stop()
	ctx := request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: "000000000000", Region: "us-east-1"}},
	)
	op := api.Descriptor(s).Operations["TranslateText"]
	for _, in := range []*api.TranslateTextRequest{
		{},
		{Text: new(strings.Repeat("a", 10001)), SourceLanguageCode: new("en"), TargetLanguageCode: new("fr")},
	} {
		if _, err := op.Invoke(ctx, in); err == nil {
			t.Fatal("model constraint accepted")
		}
	}
}

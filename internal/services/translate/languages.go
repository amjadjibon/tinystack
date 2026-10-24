package translate

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/translate"
	"github.com/amjadjibon/tinystack/internal/common"
)

// Supported languages: https://docs.aws.amazon.com/translate/latest/dg/what-is-languages.html
var languages = []api.Language{
	{LanguageName: new("Afrikaans"), LanguageCode: new("af")},
	{LanguageName: new("Albanian"), LanguageCode: new("sq")},
	{LanguageName: new("Amharic"), LanguageCode: new("am")},
	{LanguageName: new("Arabic"), LanguageCode: new("ar")},
	{LanguageName: new("Armenian"), LanguageCode: new("hy")},
	{LanguageName: new("Azerbaijani"), LanguageCode: new("az")},
	{LanguageName: new("Bengali"), LanguageCode: new("bn")},
	{LanguageName: new("Bosnian"), LanguageCode: new("bs")},
	{LanguageName: new("Bulgarian"), LanguageCode: new("bg")},
	{LanguageName: new("Catalan"), LanguageCode: new("ca")},
	{LanguageName: new("Chinese (Simplified)"), LanguageCode: new("zh")},
	{LanguageName: new("Chinese (Traditional)"), LanguageCode: new("zh-TW")},
	{LanguageName: new("Croatian"), LanguageCode: new("hr")},
	{LanguageName: new("Czech"), LanguageCode: new("cs")},
	{LanguageName: new("Danish"), LanguageCode: new("da")},
	{LanguageName: new("Dari"), LanguageCode: new("fa-AF")},
	{LanguageName: new("Dutch"), LanguageCode: new("nl")},
	{LanguageName: new("English"), LanguageCode: new("en")},
	{LanguageName: new("Estonian"), LanguageCode: new("et")},
	{LanguageName: new("Farsi (Persian)"), LanguageCode: new("fa")},
	{LanguageName: new("Filipino, Tagalog"), LanguageCode: new("tl")},
	{LanguageName: new("Finnish"), LanguageCode: new("fi")},
	{LanguageName: new("French"), LanguageCode: new("fr")},
	{LanguageName: new("French (Canada)"), LanguageCode: new("fr-CA")},
	{LanguageName: new("Georgian"), LanguageCode: new("ka")},
	{LanguageName: new("German"), LanguageCode: new("de")},
	{LanguageName: new("Greek"), LanguageCode: new("el")},
	{LanguageName: new("Gujarati"), LanguageCode: new("gu")},
	{LanguageName: new("Haitian Creole"), LanguageCode: new("ht")},
	{LanguageName: new("Hausa"), LanguageCode: new("ha")},
	{LanguageName: new("Hebrew"), LanguageCode: new("he")},
	{LanguageName: new("Hindi"), LanguageCode: new("hi")},
	{LanguageName: new("Hungarian"), LanguageCode: new("hu")},
	{LanguageName: new("Icelandic"), LanguageCode: new("is")},
	{LanguageName: new("Indonesian"), LanguageCode: new("id")},
	{LanguageName: new("Irish"), LanguageCode: new("ga")},
	{LanguageName: new("Italian"), LanguageCode: new("it")},
	{LanguageName: new("Japanese"), LanguageCode: new("ja")},
	{LanguageName: new("Kannada"), LanguageCode: new("kn")},
	{LanguageName: new("Kazakh"), LanguageCode: new("kk")},
	{LanguageName: new("Korean"), LanguageCode: new("ko")},
	{LanguageName: new("Latvian"), LanguageCode: new("lv")},
	{LanguageName: new("Lithuanian"), LanguageCode: new("lt")},
	{LanguageName: new("Macedonian"), LanguageCode: new("mk")},
	{LanguageName: new("Malay"), LanguageCode: new("ms")},
	{LanguageName: new("Malayalam"), LanguageCode: new("ml")},
	{LanguageName: new("Maltese"), LanguageCode: new("mt")},
	{LanguageName: new("Marathi"), LanguageCode: new("mr")},
	{LanguageName: new("Mongolian"), LanguageCode: new("mn")},
	{LanguageName: new("Norwegian (Bokmål)"), LanguageCode: new("no")},
	{LanguageName: new("Pashto"), LanguageCode: new("ps")},
	{LanguageName: new("Polish"), LanguageCode: new("pl")},
	{LanguageName: new("Portuguese (Brazil)"), LanguageCode: new("pt")},
	{LanguageName: new("Portuguese (Portugal)"), LanguageCode: new("pt-PT")},
	{LanguageName: new("Punjabi"), LanguageCode: new("pa")},
	{LanguageName: new("Romanian"), LanguageCode: new("ro")},
	{LanguageName: new("Russian"), LanguageCode: new("ru")},
	{LanguageName: new("Serbian"), LanguageCode: new("sr")},
	{LanguageName: new("Sinhala"), LanguageCode: new("si")},
	{LanguageName: new("Slovak"), LanguageCode: new("sk")},
	{LanguageName: new("Slovenian"), LanguageCode: new("sl")},
	{LanguageName: new("Somali"), LanguageCode: new("so")},
	{LanguageName: new("Spanish"), LanguageCode: new("es")},
	{LanguageName: new("Spanish (Mexico)"), LanguageCode: new("es-MX")},
	{LanguageName: new("Swahili"), LanguageCode: new("sw")},
	{LanguageName: new("Swedish"), LanguageCode: new("sv")},
	{LanguageName: new("Tamil"), LanguageCode: new("ta")},
	{LanguageName: new("Telugu"), LanguageCode: new("te")},
	{LanguageName: new("Thai"), LanguageCode: new("th")},
	{LanguageName: new("Turkish"), LanguageCode: new("tr")},
	{LanguageName: new("Ukrainian"), LanguageCode: new("uk")},
	{LanguageName: new("Urdu"), LanguageCode: new("ur")},
	{LanguageName: new("Uzbek"), LanguageCode: new("uz")},
	{LanguageName: new("Vietnamese"), LanguageCode: new("vi")},
	{LanguageName: new("Welsh"), LanguageCode: new("cy")},
}

func (s *Service) ListLanguages(_ context.Context, in *api.ListLanguagesRequest) (*api.ListLanguagesResponse, error) {
	if in.DisplayLanguageCode != nil && *in.DisplayLanguageCode != "en" {
		return nil, invalid("only English language names are available")
	}

	page, next, err := common.Page(
		languages,
		func(v api.Language) string { return common.Value(v.LanguageCode) },
		in.NextToken,
		limit(in.MaxResults),
		invalid("invalid NextToken"),
	)
	return &api.ListLanguagesResponse{Languages: page, NextToken: next, DisplayLanguageCode: new("en")}, err
}

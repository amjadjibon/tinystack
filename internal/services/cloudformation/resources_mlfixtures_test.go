package cloudformation

import "testing"

func TestMLResourceReplacementRules(t *testing.T) {
	for _, tc := range []struct{ typ, name, mutable string }{
		{"AWS::Translate::Terminology", "Name", "Description"},
		{"AWS::Translate::ParallelData", "Name", "ParallelDataConfig"},
		{"AWS::Transcribe::Vocabulary", "VocabularyName", "Phrases"},
		{"AWS::Transcribe::VocabularyFilter", "VocabularyFilterName", "Words"},
	} {
		t.Run(tc.typ, func(t *testing.T) {
			typ := resourceTypeOf(tc.typ)
			if typ == nil {
				t.Fatal("type not registered")
			}

			before := props{tc.name: "same", tc.mutable: "before"}
			after := props{tc.name: "same", tc.mutable: "after"}
			if typ.replaces(before, after) {
				t.Fatal("mutable property forced replacement")
			}

			after[tc.name] = "renamed"
			if !typ.replaces(before, after) {
				t.Fatal("immutable name did not replace")
			}
		})
	}
}

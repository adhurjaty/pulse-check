package main

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

// A1: minimal file parses and gets the default choices.
func TestParseMinimal(t *testing.T) {
	set, err := ParseQuestionSet([]byte(`{"title": "T", "questions": [{"text": "Q1"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := QuestionSet{Title: "T", Choices: defaultChoices, Questions: []Question{{Text: "Q1"}}}
	if !reflect.DeepEqual(set, want) {
		t.Fatalf("got %+v\nwant %+v", set, want)
	}
	// The default must be a copy, so a room can't modify the shared slice.
	set.Choices[0] = "changed"
	if defaultChoices[0] != "Fine" {
		t.Fatal("defaultChoices was modified through a parsed set")
	}
}

// A2: custom choices keep their order; whitespace is trimmed everywhere.
func TestParseCustomChoicesAndTrimming(t *testing.T) {
	set, err := ParseQuestionSet([]byte(`{
		"$schema": "./questions.schema.json",
		"title": "  Title  ",
		"choices": [" Yes ", "No", "Unsure"],
		"questions": [{"text": "  Q1 ", "detail": "  more  "}, {"text": "Q2", "detail": "   "}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	want := QuestionSet{
		Title:     "Title",
		Choices:   []string{"Yes", "No", "Unsure"},
		Questions: []Question{{Text: "Q1", Detail: "more"}, {Text: "Q2"}},
	}
	if !reflect.DeepEqual(set, want) {
		t.Fatalf("got %+v\nwant %+v", set, want)
	}
}

// A3: invalid files are rejected with every problem listed, with exact messages.
func TestParseInvalid(t *testing.T) {
	long := func(n int) string { return strings.Repeat("x", n) }
	many := func(n int) string {
		qs := make([]string, n)
		for i := range qs {
			qs[i] = `{"text": "q"}`
		}
		return `{"title": "T", "questions": [` + strings.Join(qs, ",") + `]}`
	}

	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"empty file", ``, []string{"the file is empty"}},
		{"truncated", `{"title": "T", "questions": [`, []string{
			"unexpected end of file: a closing bracket, brace or quote is probably missing"}},
		{"trailing comma", "{\n  \"title\": \"x\",\n}", []string{
			"line 3, column 1: invalid JSON: invalid character '}' looking for beginning of object key string"}},
		{"trailing content", "{\"title\": \"T\", \"questions\": [{\"text\": \"q\"}]}\n{}", []string{
			"line 2, column 1: unexpected content after the JSON object"}},
		{"not an object", `["a"]`, []string{"the file must contain a JSON object"}},
		{"missing everything", `{}`, []string{"title: is required", "questions: is required"}},
		{"blank title, empty questions", `{"title": "  ", "questions": []}`, []string{
			"title: must not be blank", "questions: must have at least 1 question"}},
		{"wrong types", `{"title": 5, "choices": "Yes", "questions": {"text": "q"}}`, []string{
			"title: must be a string", "choices: must be a list of strings", "questions: must be a list"}},
		{"unknown fields at both levels", `{"titel": "T", "title": "T", "questions": [{"text": "a"}, {"txt": "b"}]}`, []string{
			`unknown field "titel"`,
			`questions[1]: unknown field "txt"`,
			"questions[1].text: is required"}},
		{"question problems", `{"title": "T", "questions": ["q", {"text": " "}, {"text": "` + long(501) + `", "detail": "` + long(1001) + `"}, {"text": "ok", "detail": 3}]}`, []string{
			`questions[0]: must be an object with a "text" field`,
			"questions[1].text: must not be blank",
			"questions[2].text: must be at most 500 characters (got 501)",
			"questions[2].detail: must be at most 1000 characters (got 1001)",
			"questions[3].detail: must be a string"}},
		{"too many questions", many(21), []string{"questions: must have at most 20 questions (got 21)"}},
		{"too few choices", `{"title": "T", "choices": ["Only"], "questions": [{"text": "q"}]}`, []string{
			"choices: must have between 2 and 6 entries (got 1)"}},
		{"too many choices", `{"title": "T", "choices": ["a","b","c","d","e","f","g"], "questions": [{"text": "q"}]}`, []string{
			"choices: must have between 2 and 6 entries (got 7)"}},
		{"bad choices", `{"title": "T", "choices": ["Yes", " ", 3, "yes "], "questions": [{"text": "q"}]}`, []string{
			"choices[1]: must not be blank",
			"choices[2]: must be a string",
			`choices[3]: duplicate of choices[0] ("yes")`}},
		{"long title", `{"title": "` + long(201) + `", "questions": [{"text": "q"}]}`, []string{
			"title: must be at most 200 characters (got 201)"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseQuestionSet([]byte(tt.input))
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("want *ValidationError, got %v", err)
			}
			if !reflect.DeepEqual(verr.Problems, tt.want) {
				t.Fatalf("problems:\n  got  %q\n  want %q", verr.Problems, tt.want)
			}
		})
	}
}

func TestValidationErrorMessage(t *testing.T) {
	err := &ValidationError{[]string{"a", "b"}}
	if got, want := err.Error(), "invalid question file:\n  a\n  b"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Lengths are counted in characters, not bytes.
func TestParseCountsCharactersNotBytes(t *testing.T) {
	text := strings.Repeat("é", maxTextLen) // 2 bytes each
	if _, err := ParseQuestionSet([]byte(`{"title": "T", "questions": [{"text": "` + text + `"}]}`)); err != nil {
		t.Fatal(err)
	}
}

// A4: the sample file in the repo is valid.
func TestSampleQuestionsFileIsValid(t *testing.T) {
	data, err := os.ReadFile("sample-questions.json")
	if err != nil {
		t.Fatal(err)
	}
	set, err := ParseQuestionSet(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Questions) != 6 {
		t.Fatalf("expected 6 sample questions, got %d", len(set.Questions))
	}
}

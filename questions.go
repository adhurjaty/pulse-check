package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	maxQuestions = 20
	maxTextLen   = 500
	maxDetailLen = 1000
	maxTitleLen  = 200
	minChoices   = 2
	maxChoices   = 6
)

// defaultChoices is used when a question file has no "choices" field.
var defaultChoices = []string{"Fine", "Fine with reservations", "Uncomfortable", "Unacceptable"}

type Question struct {
	Text   string `json:"text"`
	Detail string `json:"detail,omitempty"`
}

// QuestionSet is the contents of an uploaded question file.
// See questions.schema.json for the editor-facing definition.
type QuestionSet struct {
	Title     string     `json:"title"`
	Choices   []string   `json:"choices"`
	Questions []Question `json:"questions"`
}

// ValidationError lists every problem found in a question file.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return "invalid question file:\n  " + strings.Join(e.Problems, "\n  ")
}

// ParseQuestionSet parses and validates a question file. On failure it
// returns a *ValidationError listing every problem it found, not just the first.
func ParseQuestionSet(data []byte) (QuestionSet, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return QuestionSet{}, &ValidationError{[]string{describeJSONError(data, err)}}
	}
	if rest := bytes.TrimLeft(data[dec.InputOffset():], " \t\r\n"); len(rest) > 0 {
		line, col := lineCol(data, int64(len(data)-len(rest)))
		return QuestionSet{}, &ValidationError{[]string{
			fmt.Sprintf("line %d, column %d: unexpected content after the JSON object", line, col),
		}}
	}

	top, ok := raw.(map[string]any)
	if !ok {
		return QuestionSet{}, &ValidationError{[]string{"the file must contain a JSON object"}}
	}

	v := &validator{}
	v.unknownFields(top, "", "$schema", "title", "choices", "questions")

	set := QuestionSet{
		Title:   v.str(top, "title", "title", true, maxTitleLen),
		Choices: v.choices(top),
	}
	set.Questions = v.questions(top)

	if len(v.problems) > 0 {
		return QuestionSet{}, &ValidationError{v.problems}
	}
	return set, nil
}

type validator struct {
	problems []string
}

func (v *validator) addf(format string, args ...any) {
	v.problems = append(v.problems, fmt.Sprintf(format, args...))
}

func (v *validator) unknownFields(obj map[string]any, path string, allowed ...string) {
	var unknown []string
	for k := range obj {
		known := false
		for _, a := range allowed {
			if k == a {
				known = true
				break
			}
		}
		if !known {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	for _, k := range unknown {
		if path == "" {
			v.addf("unknown field %q", k)
		} else {
			v.addf("%s: unknown field %q", path, k)
		}
	}
}

// str reads an optional or required string field, trimmed.
func (v *validator) str(obj map[string]any, key, path string, required bool, maxLen int) string {
	val, present := obj[key]
	if !present || val == nil {
		if required {
			v.addf("%s: is required", path)
		}
		return ""
	}
	s, ok := val.(string)
	if !ok {
		v.addf("%s: must be a string", path)
		return ""
	}
	s = strings.TrimSpace(s)
	if s == "" {
		if required {
			v.addf("%s: must not be blank", path)
		}
		return ""
	}
	if n := utf8.RuneCountInString(s); n > maxLen {
		v.addf("%s: must be at most %d characters (got %d)", path, maxLen, n)
	}
	return s
}

func (v *validator) choices(top map[string]any) []string {
	val, present := top["choices"]
	if !present || val == nil {
		return append([]string(nil), defaultChoices...)
	}
	list, ok := val.([]any)
	if !ok {
		v.addf("choices: must be a list of strings")
		return nil
	}
	if len(list) < minChoices || len(list) > maxChoices {
		v.addf("choices: must have between %d and %d entries (got %d)", minChoices, maxChoices, len(list))
	}
	out := make([]string, 0, len(list))
	seen := map[string]int{}
	for i, item := range list {
		path := fmt.Sprintf("choices[%d]", i)
		s, ok := item.(string)
		if !ok {
			v.addf("%s: must be a string", path)
			continue
		}
		s = strings.TrimSpace(s)
		if s == "" {
			v.addf("%s: must not be blank", path)
			continue
		}
		if n := utf8.RuneCountInString(s); n > maxTitleLen {
			v.addf("%s: must be at most %d characters (got %d)", path, maxTitleLen, n)
		}
		key := strings.ToLower(s)
		if j, dup := seen[key]; dup {
			v.addf("%s: duplicate of choices[%d] (%q)", path, j, s)
			continue
		}
		seen[key] = i
		out = append(out, s)
	}
	return out
}

func (v *validator) questions(top map[string]any) []Question {
	val, present := top["questions"]
	if !present || val == nil {
		v.addf("questions: is required")
		return nil
	}
	list, ok := val.([]any)
	if !ok {
		v.addf("questions: must be a list")
		return nil
	}
	if len(list) == 0 {
		v.addf("questions: must have at least 1 question")
	}
	if len(list) > maxQuestions {
		v.addf("questions: must have at most %d questions (got %d)", maxQuestions, len(list))
	}
	out := make([]Question, 0, len(list))
	for i, item := range list {
		path := fmt.Sprintf("questions[%d]", i)
		obj, ok := item.(map[string]any)
		if !ok {
			v.addf(`%s: must be an object with a "text" field`, path)
			continue
		}
		v.unknownFields(obj, path, "text", "detail")
		out = append(out, Question{
			Text:   v.str(obj, "text", path+".text", true, maxTextLen),
			Detail: v.str(obj, "detail", path+".detail", false, maxDetailLen),
		})
	}
	return out
}

func describeJSONError(data []byte, err error) string {
	var syn *json.SyntaxError
	switch {
	case errors.As(err, &syn):
		line, col := lineCol(data, syn.Offset-1)
		return fmt.Sprintf("line %d, column %d: invalid JSON: %s", line, col, syn.Error())
	case errors.Is(err, io.EOF):
		return "the file is empty"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected end of file: a closing bracket, brace or quote is probably missing"
	default:
		return "invalid JSON: " + err.Error()
	}
}

// lineCol converts a byte offset into a 1-based line and column.
func lineCol(data []byte, offset int64) (line, col int) {
	if offset < 0 {
		offset = 0
	}
	if offset > int64(len(data)) {
		offset = int64(len(data))
	}
	before := data[:offset]
	line = 1 + bytes.Count(before, []byte("\n"))
	col = int(offset) - bytes.LastIndexByte(before, '\n')
	return line, col
}

package tools

import (
	"encoding/json"
	"testing"
)

func TestValidateArgumentsReportsFieldLevelSchemaErrors(t *testing.T) {
	definition := Definition{
		Name: "test",
		Schema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"name":{"type":"string","maxLength":3,"enum":["go","api"]},
				"items":{"type":"array","maxItems":1,"items":{"type":"integer"}}
			},
			"required":["name"],
			"additionalProperties":false
		}`),
	}

	issues, arguments := ValidateArguments(
		json.RawMessage(`{"name":"tool","items":["wrong",2],"extra":true}`),
		definition,
	)
	if arguments != nil || len(issues) != 4 {
		t.Fatalf("ValidateArguments() = (%v, %v), want four field errors", arguments, issues)
	}
	if issues[0].Field != "arguments.extra" || issues[0].Message != "is not allowed" {
		t.Fatalf("first issue = %+v", issues[0])
	}
	if issues[1].Field != "arguments.items" || issues[1].Message != "must contain at most 1 items" {
		t.Fatalf("second issue = %+v", issues[1])
	}
	if issues[2].Field != "arguments.items[0]" || issues[2].Message != "must be integer" {
		t.Fatalf("third issue = %+v", issues[2])
	}
	if issues[3].Field != "arguments.name" || issues[3].Message != "must be one of the allowed values" {
		t.Fatalf("fourth issue = %+v", issues[3])
	}
}

func TestValidateArgumentsAcceptsUnicodeCharacterLimit(t *testing.T) {
	definition, exists := Lookup("inspect_text")
	if !exists {
		t.Fatal("inspect_text is not registered")
	}
	issues, arguments := ValidateArguments(json.RawMessage(`{"text":"你好","mode":"characters"}`), definition)
	if len(issues) != 0 || arguments["text"] != "你好" {
		t.Fatalf("ValidateArguments() = (%v, %v), want valid Unicode text", issues, arguments)
	}
}

func TestValidateArgumentsRejectsUnsupportedSchemaKeyword(t *testing.T) {
	definition := Definition{
		Name:   "invalid",
		Schema: json.RawMessage(`{"type":"object","patternProperties":{},"additionalProperties":false}`),
	}
	issues, arguments := ValidateArguments(json.RawMessage(`{}`), definition)
	if len(issues) != 1 || issues[0].Message != "registered schema is invalid" || arguments != nil {
		t.Fatalf("ValidateArguments() = (%v, %v), want an invalid registered-schema error", issues, arguments)
	}
}

func TestWriteNoteSchemaRejectsPathSeparators(t *testing.T) {
	definition, exists := Lookup("write_note")
	if !exists {
		t.Fatal("write_note is not registered")
	}
	issues, arguments := ValidateArguments(
		json.RawMessage(`{"filename":"../escape.txt","content":"no"}`),
		definition,
	)
	if len(issues) != 1 || issues[0].Field != "arguments.filename" || arguments != nil {
		t.Fatalf("ValidateArguments() = (%v, %v), want a filename format error", issues, arguments)
	}
}

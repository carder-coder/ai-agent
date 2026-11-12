// 本文件实现工具注册 schema 所需的 JSON Schema 子集校验。
package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"unicode/utf8"
)

// FieldError 描述参数校验失败的字段路径和原因。
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

type schema struct {
	Type                 string            `json:"type"`
	Properties           map[string]schema `json:"properties"`
	Required             []string          `json:"required"`
	Items                *schema           `json:"items"`
	Enum                 []any             `json:"enum"`
	MaxLength            int               `json:"maxLength"`
	MaxItems             int               `json:"maxItems"`
	AdditionalProperties *bool             `json:"additionalProperties"`
}

// validateArguments 检查单个 JSON 值，并拒绝 schema 未允许的参数字段。
func validateArguments(raw json.RawMessage, definition Definition) ([]FieldError, any) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return []FieldError{{Field: "arguments", Message: "must be valid JSON"}}, nil
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return []FieldError{{Field: "arguments", Message: "must contain exactly one JSON value"}}, nil
	}

	var root schema
	schemaDecoder := json.NewDecoder(bytes.NewReader(definition.Schema))
	schemaDecoder.DisallowUnknownFields()
	if err := schemaDecoder.Decode(&root); err != nil || !validSchema(root) {
		return []FieldError{{Field: "arguments", Message: "registered schema is invalid"}}, nil
	}
	errors := validateValue("arguments", value, root)
	return errors, value
}

func validSchema(rule schema) bool {
	switch rule.Type {
	case "object", "array", "string", "number", "integer", "boolean", "null":
	default:
		return false
	}
	if rule.MaxLength < 0 || rule.MaxItems < 0 {
		return false
	}
	for _, property := range rule.Properties {
		if !validSchema(property) {
			return false
		}
	}
	if rule.Items != nil && !validSchema(*rule.Items) {
		return false
	}
	for _, required := range rule.Required {
		if _, exists := rule.Properties[required]; !exists {
			return false
		}
	}
	return true
}

func validateValue(path string, value any, rule schema) []FieldError {
	if !matchesType(value, rule.Type) {
		return []FieldError{{Field: path, Message: "must be " + rule.Type}}
	}

	if len(rule.Enum) > 0 {
		matched := false
		for _, option := range rule.Enum {
			if equalJSONValue(value, option) {
				matched = true
				break
			}
		}
		if !matched {
			return []FieldError{{Field: path, Message: "must be one of the allowed values"}}
		}
	}

	var errors []FieldError
	switch typed := value.(type) {
	case map[string]any:
		for _, key := range rule.Required {
			if _, exists := typed[key]; !exists {
				errors = append(errors, FieldError{Field: path + "." + key, Message: "is required"})
			}
		}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			property, exists := rule.Properties[key]
			if !exists {
				if rule.AdditionalProperties != nil && !*rule.AdditionalProperties {
					errors = append(errors, FieldError{Field: path + "." + key, Message: "is not allowed"})
				}
				continue
			}
			errors = append(errors, validateValue(path+"."+key, typed[key], property)...)
		}
	case string:
		if rule.MaxLength > 0 && utf8.RuneCountInString(typed) > rule.MaxLength {
			errors = append(errors, FieldError{
				Field: path, Message: fmt.Sprintf("must contain at most %d characters", rule.MaxLength),
			})
		}
	case []any:
		if rule.MaxItems > 0 && len(typed) > rule.MaxItems {
			errors = append(errors, FieldError{
				Field: path, Message: fmt.Sprintf("must contain at most %d items", rule.MaxItems),
			})
		}
		if rule.Items != nil {
			for index, item := range typed {
				errors = append(errors, validateValue(fmt.Sprintf("%s[%d]", path, index), item, *rule.Items)...)
			}
		}
	}
	return errors
}

func matchesType(value any, expected string) bool {
	switch expected {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		_, ok := value.(json.Number)
		return ok
	case "integer":
		number, ok := value.(json.Number)
		if !ok {
			return false
		}
		_, err := number.Int64()
		return err == nil
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	default:
		return false
	}
}

func equalJSONValue(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

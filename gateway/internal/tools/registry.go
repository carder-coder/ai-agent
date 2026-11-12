// 本文件集中声明可调用工具及其参数 schema，调用方不能动态注册可执行文件。
package tools

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Definition 描述一个服务端预先注册的工具及其 JSON Schema 参数契约。
type Definition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"parameters"`
	Write       bool            `json:"write"`
}

// definitions 是唯一可调用工具清单；新增工具必须同时提供受限 schema 与固定执行逻辑。
var definitions = map[string]Definition{
	"inspect_text": {
		Name:        "inspect_text",
		Description: "统计文本字符数和词数，不会保存或转发文本。",
		Schema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"text":{"type":"string","maxLength":4096},
				"mode":{"type":"string","enum":["summary","characters","words"],"maxLength":10}
			},
			"required":["text"],
			"additionalProperties":false
		}`),
	},
}

// Lookup 返回已注册工具的定义；未知名称不会触发动态命令执行。
func Lookup(name string) (Definition, bool) {
	definition, exists := definitions[name]
	return definition, exists
}

// ValidateArguments 返回字段级校验错误以及解码后的参数对象。
func ValidateArguments(raw json.RawMessage, definition Definition) ([]FieldError, map[string]any) {
	errors, value := validateArguments(raw, definition)
	if errors != nil {
		return errors, nil
	}
	arguments, ok := value.(map[string]any)
	if !ok {
		return []FieldError{{Field: "arguments", Message: "must be an object"}}, nil
	}
	return nil, arguments
}

// Execute 运行纯计算工具；具有副作用的工具由独立审批流程控制。
func Execute(name string, arguments map[string]any) (map[string]any, error) {
	switch name {
	case "inspect_text":
		text, ok := arguments["text"].(string)
		if !ok {
			return nil, fmt.Errorf("validated text parameter has unexpected type")
		}
		mode, _ := arguments["mode"].(string)
		characters := len([]rune(text))
		words := len(strings.Fields(text))
		switch mode {
		case "characters":
			return map[string]any{"characters": characters}, nil
		case "words":
			return map[string]any{"words": words}, nil
		default:
			return map[string]any{"characters": characters, "words": words}, nil
		}
	default:
		return nil, fmt.Errorf("tool %q has no executor", name)
	}
}

// Names 返回排序后的工具名，保证文档和接口输出顺序稳定。
func Names() []string {
	names := make([]string, 0, len(definitions))
	for name := range definitions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

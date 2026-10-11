package common

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"
)

func Unmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// UnmarshalStrictObject 用于封闭控制合同：拒绝重复键、null、非对象、未知字段与尾随内容。
func UnmarshalStrictObject(data []byte, v any) error {
	if !utf8.Valid(data) {
		return fmt.Errorf("JSON must contain valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return fmt.Errorf("JSON must be an object")
	}
	if err := readStrictJSONContainer(decoder, json.Delim('{'), 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("JSON has trailing content")
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(v)
}

func readStrictJSONContainer(decoder *json.Decoder, start json.Delim, depth int) error {
	if depth > 64 {
		return fmt.Errorf("JSON nesting exceeds limit")
	}
	keys := map[string]bool{}
	for decoder.More() {
		if start == '{' {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || keys[name] {
				return fmt.Errorf("JSON contains a duplicate or invalid object key")
			}
			keys[name] = true
		}
		value, err := decoder.Token()
		if err != nil {
			return err
		}
		if value == nil {
			return fmt.Errorf("JSON must not contain null")
		}
		if delimiter, ok := value.(json.Delim); ok {
			if err := readStrictJSONContainer(decoder, delimiter, depth+1); err != nil {
				return err
			}
		}
	}
	end, err := decoder.Token()
	if err != nil || (start == '{' && end != json.Delim('}')) || (start == '[' && end != json.Delim(']')) {
		return fmt.Errorf("JSON container is invalid")
	}
	return nil
}

func UnmarshalJsonStr(data string, v any) error {
	return json.Unmarshal(StringToByteSlice(data), v)
}

func DecodeJson(reader io.Reader, v any) error {
	return json.NewDecoder(reader).Decode(v)
}

func Marshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

func IndentJson(data []byte) ([]byte, error) {
	var buffer bytes.Buffer
	if err := json.Indent(&buffer, data, "", "  "); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func GetJsonType(data json.RawMessage) string {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return "unknown"
	}
	firstChar := trimmed[0]
	switch firstChar {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	default:
		return "number"
	}
}

// JsonRawMessageToString returns JSON strings as their decoded value and other JSON values as raw text.
func JsonRawMessageToString(data json.RawMessage) string {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	if trimmed[0] != '"' {
		return string(trimmed)
	}
	var value string
	if err := Unmarshal(trimmed, &value); err != nil {
		return string(trimmed)
	}
	return value
}

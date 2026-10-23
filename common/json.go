package common

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"

	kitutil "github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
	"github.com/gin-gonic/gin/binding"
)

type RawMessage = json.RawMessage

// hostJSONCodec is the single place where the host chooses its JSON engine.
// Swap the implementation here (for example to sonic.ConfigStd) and every
// common.* and kitutil.* JSON helper, including relaykit DTO (un)marshalling,
// follows. Injected from init() rather than main() so tests run on the same
// engine as production: common is imported by virtually every root package
// and test binary, while main() never executes under `go test`.
type hostJSONCodec struct{}

func (hostJSONCodec) Marshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

func (hostJSONCodec) Unmarshal(data []byte, v any) error {
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

func (hostJSONCodec) Decode(r io.Reader, v any) error {
	return json.NewDecoder(r).Decode(v)
}

func (hostJSONCodec) Valid(data []byte) bool {
	return json.Valid(data)
}

func init() {
	kitutil.SetCodec(hostJSONCodec{})
}

func Unmarshal(data []byte, v any) error {
	return kitutil.Unmarshal(data, v)
}

func UnmarshalJsonStr(data string, v any) error {
	return kitutil.UnmarshalJsonStr(data, v)
}

func DecodeJson(reader io.Reader, v any) error {
	return kitutil.DecodeJson(reader, v)
}

// DecodeJsonWithValidation decodes JSON and applies Gin's configured binding-tag
// validator, including binding:"required" and any registered custom validators.
func DecodeJsonWithValidation(reader io.Reader, v any) error {
	if err := DecodeJson(reader, v); err != nil {
		return err
	}
	if binding.Validator == nil {
		return nil
	}
	return binding.Validator.ValidateStruct(v)
}

func Marshal(v any) ([]byte, error) {
	return kitutil.Marshal(v)
}

func IndentJson(data []byte) ([]byte, error) {
	var buffer bytes.Buffer
	if err := json.Indent(&buffer, data, "", "  "); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func GetJsonType(data RawMessage) string {
	return kitutil.GetJsonType(data)
}

// JsonRawMessageToString returns JSON strings as their decoded value and other JSON values as raw text.
func JsonRawMessageToString(data RawMessage) string {
	return kitutil.JsonRawMessageToString(data)
}

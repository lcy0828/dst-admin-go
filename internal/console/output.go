package console

import (
	"strings"
	"unicode/utf8"
)

const maxCommandOutputBytes = 16 << 10

func commandOutput(details map[string]interface{}) *Output {
	value, ok := details["output"].(map[string]interface{})
	if !ok {
		return nil
	}
	text, ok := value["text"].(string)
	truncated, valid := value["truncated"].(bool)
	if !ok || !valid {
		return nil
	}
	text = strings.ToValidUTF8(text, "�")
	if len(text) > maxCommandOutputBytes {
		text = text[:maxCommandOutputBytes]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
		truncated = true
	}
	return &Output{Text: text, Truncated: truncated}
}

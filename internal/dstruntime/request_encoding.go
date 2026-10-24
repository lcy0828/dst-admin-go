package dstruntime

import (
	"bytes"
	"encoding/json"
)

// DST's json.decode evaluates string literals with Lua 5.1's loadstring. It
// does not implement JSON's Unicode escapes: \u003e becomes the text u003e.
// Keep HTML characters in their original UTF-8 form for both direct console
// delivery and request documents. HTTP/Agent JSON encoding remains unchanged.
func marshalRuntimeRequest(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	payload := bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'})
	// The standard encoder always escapes these two Unicode separators, even
	// with HTML escaping disabled. UTF-8 is valid JSON here. Walk escape pairs
	// so a user's literal "\\u2028" is never mistaken for an encoded character.
	if !bytes.Contains(payload, []byte(`\u202`)) {
		return payload, nil
	}
	result := make([]byte, 0, len(payload))
	for i := 0; i < len(payload); i++ {
		if payload[i] == '\\' && i+1 < len(payload) {
			if i+6 <= len(payload) && (string(payload[i:i+6]) == `\u2028` || string(payload[i:i+6]) == `\u2029`) {
				last := byte(0xa8)
				if payload[i+5] == '9' {
					last = 0xa9
				}
				result = append(result, 0xe2, 0x80, last)
				i += 5
				continue
			}
			result = append(result, payload[i], payload[i+1])
			i++
			continue
		}
		result = append(result, payload[i])
	}
	return result, nil
}

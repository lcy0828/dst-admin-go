package rooms

import "testing"

func TestValidateClusterToken(t *testing.T) {
	for _, test := range []struct {
		name       string
		value      string
		allowEmpty bool
		valid      bool
	}{
		{name: "valid", value: "pds-g^complete-token-value", valid: true},
		{name: "future format", value: "future_token_format_123", valid: true},
		{name: "empty optional", allowEmpty: true, valid: true},
		{name: "empty required", valid: false},
		{name: "too short", value: "expired", valid: false},
		{name: "whitespace", value: "pds-g^complete token value", valid: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateClusterToken(test.value, test.allowEmpty)
			if (err == nil) != test.valid {
				t.Fatalf("error = %v, valid = %t", err, test.valid)
			}
		})
	}
}

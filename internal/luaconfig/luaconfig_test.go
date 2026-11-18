package luaconfig

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeSourceStripsBOMAndKleiHeader(t *testing.T) {
	input := "\xEF\xBB\xBFKLEI     1 return {\r\n  -- keep user comment\r\n  location = \"forest\",\r\n  overrides = {\r\n    autumn = \"longseason\",\r\n  },\r\n}"
	normalized, err := NormalizeSource([]byte(input), NormalizeOptions{MaxBytes: 64 * 1024, AllowBareTable: true})
	if err != nil {
		t.Fatalf("NormalizeSource error = %v", err)
	}
	if !normalized.StrippedBOM || !normalized.StrippedKleiHeader || normalized.WrappedReturn {
		t.Fatalf("unexpected flags: %+v", normalized)
	}
	text := string(normalized.Data)
	if !strings.HasPrefix(text, "return {\n") {
		t.Fatalf("expected clean return header, got %q", text)
	}
	if !strings.Contains(text, "-- keep user comment") {
		t.Fatalf("expected Lua comment preserved, got %q", text)
	}
	if err := ValidateLiteralReturnTable(normalized.Data, "leveldataoverride.lua", 64*1024); err != nil {
		t.Fatalf("ValidateLiteralReturnTable error = %v", err)
	}
}

func TestNormalizeSourceWrapsBareTablePreservingLeadingComments(t *testing.T) {
	input := "-- pasted from community guide\n{\n  [\"workshop-378160973\"] = { enabled = true },\n}"
	normalized, err := NormalizeSource([]byte(input), NormalizeOptions{MaxBytes: 64 * 1024, AllowBareTable: true})
	if err != nil {
		t.Fatalf("NormalizeSource error = %v", err)
	}
	if !normalized.WrappedReturn {
		t.Fatalf("expected WrappedReturn=true")
	}
	if err := ValidateLiteralReturnTable(normalized.Data, "modoverrides.lua", 64*1024); err != nil {
		t.Fatalf("ValidateLiteralReturnTable error = %v", err)
	}
}

func TestValidateLiteralReturnTableRejectsDynamicCodeAndReportsLine(t *testing.T) {
	cases := []struct {
		name     string
		source   string
		wantLine int
	}{
		{
			name:     "infinite loop DoS",
			source:   "while true do end\nreturn {}\n",
			wantLine: 2,
		},
		{
			name:     "dynamic function call in table",
			source:   "return {\n  overrides = {\n    day = os.execute('id'),\n  },\n}\n",
			wantLine: 3,
		},
		{
			name:     "duplicate key",
			source:   "return {\n  [\"workshop-100\"] = { enabled = true },\n  [\"workshop-100\"] = { enabled = false },\n}\n",
			wantLine: 3,
		},
		{
			name:     "syntax error",
			source:   "return {\n  location = \"forest\"\n  overrides = {}\n}\n",
			wantLine: 3,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateLiteralReturnTable([]byte(tc.source), "test.lua", 64*1024)
			if err == nil {
				t.Fatalf("expected validation error for %s", tc.name)
			}
			var syntaxErr *SyntaxError
			if !errors.As(err, &syntaxErr) {
				t.Fatalf("expected *SyntaxError, got %T (%v)", err, err)
			}
			if syntaxErr.Line != tc.wantLine {
				t.Fatalf("line = %d, want %d (message=%q)", syntaxErr.Line, tc.wantLine, syntaxErr.Message)
			}
		})
	}
}

func TestValidateNormalizedReturnTablePreservesOriginalLineAfterMultilineKleiHeader(t *testing.T) {
	raw := "KLEI     1\nreturn {\n  location = \"forest\"\n  overrides = {}\n}\n"
	normalized, err := NormalizeSource([]byte(raw), NormalizeOptions{MaxBytes: 64 * 1024, AllowBareTable: true})
	if err != nil {
		t.Fatalf("NormalizeSource error = %v", err)
	}
	if normalized.RemovedLines != 1 {
		t.Fatalf("RemovedLines = %d, want 1", normalized.RemovedLines)
	}
	valErr := ValidateNormalizedReturnTable(normalized, "leveldataoverride.lua", 64*1024)
	if valErr == nil {
		t.Fatalf("expected syntax error")
	}
	var syntaxErr *SyntaxError
	if !errors.As(valErr, &syntaxErr) {
		t.Fatalf("expected *SyntaxError, got %T", valErr)
	}
	if syntaxErr.Line != 4 {
		t.Fatalf("syntaxErr.Line = %d, want 4 (raw buffer line of 'overrides = {}')", syntaxErr.Line)
	}
}


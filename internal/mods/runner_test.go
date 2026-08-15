package mods

import (
	"strings"
	"testing"
)

func TestSteamCMDOutputFailed(t *testing.T) {
	tests := []struct {
		name   string
		output string
		failed bool
	}{
		{name: "success", output: "Success. Downloaded item 1392778117", failed: false},
		{name: "download failure", output: "ERROR! Download item 1392778117 failed (I/O Operation Failed).", failed: true},
		{name: "staging failure", output: "Update canceled: Staging folder not writable (Disk write failure)", failed: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if actual := steamCMDOutputFailed(test.output); actual != test.failed {
				t.Fatalf("steamCMDOutputFailed() = %v, want %v", actual, test.failed)
			}
		})
	}
}

func TestBoundedTailWriter(t *testing.T) {
	writer := &boundedTailWriter{limit: 8}
	for _, value := range []string{"abc", "def", "ghijk"} {
		if count, err := writer.Write([]byte(value)); err != nil || count != len(value) {
			t.Fatalf("Write(%q) = %d, %v", value, count, err)
		}
	}
	if actual := writer.String(); actual != "defghijk" {
		t.Fatalf("unexpected tail %q", actual)
	}
	long := strings.Repeat("x", 12)
	_, _ = writer.Write([]byte(long))
	if actual := writer.String(); actual != strings.Repeat("x", 8) {
		t.Fatalf("unexpected replacement tail %q", actual)
	}
}

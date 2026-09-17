package luajit

import (
	"errors"
	"strings"
	"testing"
)

func TestDependencyCheckRejectsMissingVersionsEvenWhenLddSucceeds(t *testing.T) {
	for _, output := range []string{"libstdc++.so.6: version `GLIBCXX_3.4.32' not found", "libbfd.so => not found"} {
		if err := dependencyResult(output, nil); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("accepted incompatible output: %s", output)
		}
	}
	if err := dependencyResult("libc.so.6 => /lib/libc.so.6 (0x1234)", nil); err != nil {
		t.Fatal(err)
	}
	if err := dependencyResult("", errors.New("ldd unavailable")); err == nil {
		t.Fatal("silently skipped dependency check")
	}
}

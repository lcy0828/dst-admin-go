package modmetadata

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
)

// Synthetic data: exercise large, translated configuration tables without
// redistributing third-party mod source or depending on a network download.
func TestLargeTranslatedConfigurationKeepsFinalMetadata(t *testing.T) {
	var source strings.Builder
	source.WriteString(`name=ChooseTranslationTable({zh="测试模组",en="Example"});version="old";configuration_options={`)
	for i := 0; i < 1100; i++ {
		fmt.Fprintf(&source, `{name="option_%d",label="Option",options={{description="Yes",data=true},{description="No",data=false}},default=true},`, i)
	}
	source.WriteString(`};version="2.0";author="Example";dst_compatible=true;api_version=10`)
	path := filepath.Join(t.TempDir(), "modinfo.lua")
	if err := os.WriteFile(path, []byte(source.String()), 0600); err != nil {
		t.Fatal(err)
	}
	var got Metadata
	// Verify large-table semantics independently of race instrumentation and
	// shared CI CPU scheduling. Production keeps its real 100ms deadline;
	// the VM instruction and allocation limits still apply inside this bubble.
	synctest.Test(t, func(t *testing.T) {
		got = Read(context.Background(), path, "123", "zh")
	})
	if got.Reason != "" || got.Version != "2.0" || got.Name != "测试模组" {
		t.Fatalf("large valid config lost identity (%d bytes): %+v", source.Len(), got)
	}
}

func TestFailedEvaluationDoesNotReturnPartialVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "modinfo.lua")
	if err := os.WriteFile(path, []byte(`name="Example";version="1";while true do end;version="2"`), 0600); err != nil {
		t.Fatal(err)
	}
	got := Read(context.Background(), path, "123", "zh")
	if got.Reason == "" || got.Version != "" || got.Name != "" {
		t.Fatalf("partial identity accepted: %+v", got)
	}
}

func TestDescriptionFormatBoundsActualExpansion(t *testing.T) {
	for _, test := range []struct {
		format string
		valid  bool
	}{
		{`name="Example";version="2";description=string.format("%s %s %s %s %s %s %s %s %s", "a","b","c","d","e","f","g","h","i")`, true},
		{`name="Example";description=string.format("%9999s%9999s%9999s%9999s%9999s%9999s%9999s", "a","a","a","a","a","a","a")`, false},
		{`name="Example";description=string.format("%1000000000s", "a")`, false},
	} {
		path := filepath.Join(t.TempDir(), "modinfo.lua")
		if err := os.WriteFile(path, []byte(test.format), 0600); err != nil {
			t.Fatal(err)
		}
		got := Read(context.Background(), path, "123", "en")
		if (got.Reason == "") != test.valid {
			t.Fatalf("valid=%t: %+v", test.valid, got)
		}
	}
}

func TestDSTAPIVersionOverridesSinglePlayerCompatibility(t *testing.T) {
	for _, prefix := range []string{"", "local x=true;"} {
		path := filepath.Join(t.TempDir(), "modinfo.lua")
		if err := os.WriteFile(path, []byte(prefix+`name="Example";version="1";api_version=6;api_version_dst=10;dst_compatible=true`), 0600); err != nil {
			t.Fatal(err)
		}
		if got := Read(context.Background(), path, "123", "en"); got.APIVersion != "10" || got.Reason != "" {
			t.Fatal(got)
		}
	}
}

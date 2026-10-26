package modmetadata

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMetadataTranslationsDependenciesAndChangedFiles(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "modinfo.lua")
	dependency := filepath.Join(root, "identity.lua")
	if err := os.WriteFile(path, []byte(`local item = require("identity"); name=ChooseTranslationTable({zh=item.zh,en="English"});author=[[作者]];version=item.version`), 0600); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(dependency, []byte(`return {zh="本地模组",version="1.0"}`), 0600)
	got := Read(context.Background(), path, "local:fishdried_custom", "zh")
	if got.Name != "本地模组" || got.Version != "1.0" || got.Author != "作者" {
		t.Fatal(got)
	}
	if english := Read(context.Background(), path, "local:fishdried_custom", "en"); english.Name != "English" {
		t.Fatal(english)
	}
	info, _ := os.Stat(dependency)
	_ = os.WriteFile(dependency, []byte(`return {zh="新的模组",version="2.0"}`), 0600)
	_ = os.Chtimes(dependency, info.ModTime(), info.ModTime())
	got = Read(context.Background(), path, "local:fishdried_custom", "zh")
	if got.Name != "新的模组" || got.Version != "2.0" {
		t.Fatal("stale cached dependency", got)
	}
}
func TestMetadataDoesNotReadOutsideRootAndBoundsLoops(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "modinfo.lua")
	for _, code := range []string{`name=require("../secret")`, `while true do end`} {
		_ = os.WriteFile(path, []byte(code), 0600)
		start := time.Now()
		got := Read(context.Background(), path, "local:test", "zh")
		if got.Reason == "" || time.Since(start) > time.Second {
			t.Fatal(got, time.Since(start))
		}
	}
}
func TestStaticMetadataIgnoresCommentsAndPreservesLongStrings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "modinfo.lua")
	_ = os.WriteFile(path, []byte("-- name='wrong'\nname=[[中文名称]]\nauthor='作者'\nversion='1.2'\nconfiguration_options={}\n"), 0600)
	got := Read(context.Background(), path, "local:test", "zh")
	if got.Name != "中文名称" || got.Author != "作者" || got.Version != "1.2" {
		t.Fatal(got)
	}
}
func BenchmarkMetadataWarm200(b *testing.B) {
	root := b.TempDir()
	paths := make([]string, 200)
	for i := range paths {
		paths[i] = filepath.Join(root, fmt.Sprintf("mod-%d.lua", i))
		_ = os.WriteFile(paths[i], []byte(`name="模组";author="作者";version="1.2"`), 0600)
		Read(context.Background(), paths[i], "local:test", "zh")
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		for _, path := range paths {
			Read(context.Background(), path, "local:test", "zh")
		}
	}
}

func TestMetadataBoundsNativeAllocationsAndConcatenation(t *testing.T) {
	for _, code := range []string{
		`local s="abcd";for i=1,40 do s=s..s end;name=s`,
		`name=string.format("%1000000000s","x")`,
		`local t={}; t[60000000]="x";name="x"`,
		`local t={[60000000]="x"};name=t[60000000]`,
		`local t={};table.insert(t,60000000,"x");name="x"`,
	} {
		path := filepath.Join(t.TempDir(), "modinfo.lua")
		if err := os.WriteFile(path, []byte(code), 0600); err != nil {
			t.Fatal(err)
		}
		got := Read(context.Background(), path, "local:limits", "zh")
		if got.Reason == "" {
			t.Fatalf("allocation escaped: %s: %+v", code, got)
		}
	}
	path := filepath.Join(t.TempDir(), "modinfo.lua")
	_ = os.WriteFile(path, []byte(`local label=locale=="zh" and "测试" or "test";name=label.." mod";version=string.format("%d.%d",1,2)`), 0600)
	if got := Read(context.Background(), path, "local:normal", "zh"); got.Name != "测试 mod" || got.Version != "1.2" {
		t.Fatalf("ordinary expressions rejected: %+v", got)
	}
}

func BenchmarkMetadataCold(b *testing.B) {
	for _, count := range []int{50, 200} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			root := b.TempDir()
			paths := make([]string, count)
			for i := range paths {
				paths[i] = filepath.Join(root, fmt.Sprintf("mod-%d.lua", i))
				code := `name="模组";author="作者";version="1.2"`
				if i%4 == 0 {
					code = `name=ChooseTranslationTable({zh="模组",en="Mod"}).." 1.2";author="作者";version="1.2"`
				}
				if err := os.WriteFile(paths[i], []byte(code), 0600); err != nil {
					b.Fatal(err)
				}
			}
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				cache.Lock()
				clear(cache.items)
				cache.Unlock()
				for _, path := range paths {
					if got := Read(context.Background(), path, "local:test", "zh"); got.Name == "" {
						b.Fatal(got)
					}
				}
			}
		})
	}
}

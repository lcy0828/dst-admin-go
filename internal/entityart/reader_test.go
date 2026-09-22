package entityart

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const sampleAtlas = `<Atlas><Texture filename="icons.tex"/><Elements><Element name="log.tex" u1="0" u2="1" v1="0" v2="1"/></Elements></Atlas>`

func texture() []byte {
	data := make([]byte, 18)
	copy(data, "KTEX")
	binary.LittleEndian.PutUint32(data[4:], 4<<4|1<<13)
	binary.LittleEndian.PutUint16(data[8:], 2)
	binary.LittleEndian.PutUint16(data[10:], 2)
	binary.LittleEndian.PutUint32(data[14:], 16)
	return append(data, []byte{255, 0, 0, 255, 255, 0, 0, 255, 0, 255, 0, 255, 0, 255, 0, 255}...)
}
func fixture(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "images"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "images/inventory.xml"), []byte(sampleAtlas), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "images/icons.tex"), texture(), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestModArtworkIsCroppedFlippedAndCachedWithoutLua(t *testing.T) {
	root := t.TempDir()
	fixture(t, filepath.Join(root, "mods/workshop-123"))
	r := NewReader()
	data, err := r.Read(context.Background(), root, "", "log", "workshop-123")
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != 2 || color.NRGBAModel.Convert(img.At(0, 0)) != (color.NRGBA{0, 255, 0, 255}) {
		t.Fatalf("wrong crop or orientation: %v %v", img.Bounds(), img.At(0, 0))
	}
	// A subsequent request reuses the cache even if an update replaces the file.
	os.WriteFile(filepath.Join(root, "mods/workshop-123/images/icons.tex"), []byte("partial update"), 0600)
	again, err := r.Read(context.Background(), root, "", "log", "workshop-123")
	if err != nil || !bytes.Equal(data, again) {
		t.Fatalf("cache miss: %v", err)
	}
	data[0] = 0
	if ValidatePNG(again) != nil {
		t.Fatal("cache payload was mutable by caller")
	}
	for _, c := range r.collections {
		c.until = time.Now().Add(-time.Second)
	}
	if _, err = r.Read(context.Background(), root, "", "log", "workshop-123"); err == nil {
		t.Fatal("expired thumbnail was not invalidated")
	}
}
func TestVanillaBundledAtlasAndModIsolation(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "data/databundles"), 0750)
	f, err := os.Create(filepath.Join(root, "data/databundles/images.zip"))
	if err != nil {
		t.Fatal(err)
	}
	z := zip.NewWriter(f)
	for name, data := range map[string][]byte{"images/inventoryimages1.xml": []byte(sampleAtlas), "images/icons.tex": texture()} {
		w, _ := z.Create(name)
		w.Write(data)
	}
	z.Close()
	f.Close()
	r := NewReader()
	data, err := r.Read(context.Background(), root, "", "log", "")
	if err != nil || len(data) == 0 {
		t.Fatalf("%v", err)
	}
	data, err = r.Read(context.Background(), root, "", "log", "workshop-123")
	if err != nil || len(data) != 0 {
		t.Fatal("mod override borrowed vanilla artwork", err)
	}
	data, err = r.Read(context.Background(), root, "", "missing", "")
	if err != nil || len(data) != 0 {
		t.Fatal("missing prefab should have no image", err)
	}
}
func TestArtworkUsesConfiguredUGCAndConfinesTexturePaths(t *testing.T) {
	root, ugc := t.TempDir(), t.TempDir()
	fixture(t, filepath.Join(ugc, "123"))
	r := NewReader()
	data, err := r.Read(context.Background(), root, ugc, "log", "workshop-123")
	if err != nil || len(data) == 0 {
		t.Fatal("UGC ignored", err)
	}
	outside := filepath.Join(t.TempDir(), "secret.tex")
	os.WriteFile(outside, texture(), 0600)
	os.Remove(filepath.Join(ugc, "123/images/icons.tex"))
	os.Symlink(outside, filepath.Join(ugc, "123/images/icons.tex"))
	r = NewReader()
	if _, err = r.Read(context.Background(), root, ugc, "log", "workshop-123"); err == nil {
		t.Fatal("read outside mod root")
	}
	for _, value := range []string{"../secret", "/tmp/secret", "a/b", "..", "a\\b"} {
		if Valid("log", value) || Valid(value, "") {
			t.Fatal("unsafe identifier", value)
		}
	}
}
func TestArtworkCancellationAndSizeLimits(t *testing.T) {
	r := NewReader()
	r.gate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Read(ctx, t.TempDir(), "", "log", ""); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	<-r.gate
	root := t.TempDir()
	mod := filepath.Join(root, "mods/localmod")
	fixture(t, mod)
	large := texture()
	binary.LittleEndian.PutUint16(large[8:], 16384)
	os.WriteFile(filepath.Join(mod, "images/icons.tex"), large, 0600)
	if _, err := r.Read(context.Background(), root, "", "log", "localmod"); !errors.Is(err, ErrLimit) {
		t.Fatal("oversized decode not blocked", err)
	}
	if ValidatePNG([]byte("not a PNG")) == nil {
		t.Fatal("accepted non PNG output")
	}
}

func TestBadAtlasAndTextureAreNegativelyCached(t *testing.T) {
	root := t.TempDir()
	mod := filepath.Join(root, "mods/localmod")
	fixture(t, mod)
	os.WriteFile(filepath.Join(mod, "images/icons.tex"), []byte("broken"), 0600)
	r := NewReader()
	if _, err := r.Read(context.Background(), root, "", "log", "localmod"); err == nil {
		t.Fatal("bad texture accepted")
	}
	os.WriteFile(filepath.Join(mod, "images/icons.tex"), texture(), 0600)
	if _, err := r.Read(context.Background(), root, "", "log", "localmod"); err == nil {
		t.Fatal("broken texture was re-read instead of caching the failure")
	}
	for _, c := range r.collections {
		c.until = time.Now().Add(-time.Second)
	}
	if data, err := r.Read(context.Background(), root, "", "log", "localmod"); err != nil || len(data) == 0 {
		t.Fatal("failed texture never refreshed", err)
	}
	// Conflicting mod atlases must not pick an arbitrary picture for one Prefab.
	os.WriteFile(filepath.Join(mod, "images/duplicate.xml"), []byte(`<Atlas><Texture filename="other.tex"/><Elements><Element name="log.tex" u1="0" u2="1" v1="0" v2="1"/></Elements></Atlas>`), 0600)
	r = NewReader()
	if data, err := r.Read(context.Background(), root, "", "log", "localmod"); err != nil || len(data) != 0 {
		t.Fatal("ambiguous mod icon was guessed", err)
	}
}

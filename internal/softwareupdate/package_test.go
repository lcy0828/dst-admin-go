package softwareupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestExtractorRejectsUnsafeAndCorruptArchives(t *testing.T) {
	for _, name := range []string{"../outside", "/dst-admin", "dst-admin/../../outside", "extra", "dst-admin-link", "duplicate", "oversize", "corrupt"} {
		t.Run(name, func(t *testing.T) {
			var archive bytes.Buffer
			gz := gzip.NewWriter(&archive)
			writer := tar.NewWriter(gz)
			header := &tar.Header{Name: name, Size: 1, Mode: 0700, Typeflag: tar.TypeReg}
			if name == "dst-admin-link" {
				header.Name = "dst-admin"
				header.Typeflag = tar.TypeSymlink
				header.Linkname = "../outside"
				header.Size = 0
			}
			if name == "oversize" {
				header.Name = "dst-admin"
				header.Size = MaxExtractedBytes + 1
			}
			if name == "duplicate" {
				header.Name = "dst-admin"
			}
			_ = writer.WriteHeader(header)
			if header.Size == 1 {
				_, _ = writer.Write([]byte("x"))
			}
			if name == "duplicate" {
				_ = writer.WriteHeader(header)
				_, _ = writer.Write([]byte("y"))
			}
			_ = writer.Close()
			_ = gz.Close()
			data := archive.Bytes()
			if name == "corrupt" {
				data = testBundle(t, "v1.1.0", "linux-amd64", nil)
				data[len(data)-8] ^= 0xff
			}
			root := t.TempDir()
			path := filepath.Join(root, "archive.gz")
			_ = os.WriteFile(path, data, 0600)
			stage := filepath.Join(root, "stage")
			_ = os.Mkdir(stage, 0700)
			if err := extractBundle(path, stage); err == nil {
				t.Fatal("unsafe archive accepted")
			}
			if _, err := os.Stat(filepath.Join(root, "outside")); !os.IsNotExist(err) {
				t.Fatal("wrote outside stage")
			}
		})
	}
}

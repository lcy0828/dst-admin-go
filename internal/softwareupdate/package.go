package softwareupdate

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"debug/buildinfo"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	appbuild "dont/internal/buildinfo"
)

func extractBundle(archive, directory string) error {
	return extractBundleFor(archive, directory, "", "")
}

func extractBundleFor(archive, directory, kind, platform string) error {
	files := filesFor(kind, platform)
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	reader := tar.NewReader(io.LimitReader(gz, MaxExtractedBytes+(1<<20)))
	seen := map[string]bool{}
	var total int64
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if header.Typeflag == tar.TypeDir && (header.Name == "./" || header.Name == ".") {
			continue
		}
		name := strings.TrimPrefix(header.Name, "./")
		if (!files[name] && name != "manifest.json") || seen[name] || header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > MaxExtractedBytes {
			return ErrInvalid
		}
		if name == "manifest.json" && header.Size > 64<<10 {
			return ErrInvalid
		}
		seen[name] = true
		total += header.Size
		if total > MaxExtractedBytes {
			return ErrInvalid
		}
		mode := os.FileMode(0700)
		if name == "manifest.json" {
			mode = 0600
		}
		output, err := os.OpenFile(filepath.Join(directory, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		n, copyErr := io.Copy(output, reader)
		if copyErr == nil {
			copyErr = output.Sync()
		}
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if n != header.Size {
			return ErrInvalid
		}
	}
	if len(seen) != len(files)+1 {
		return ErrInvalid
	}
	// Consume the gzip trailer so truncated/corrupt streams cannot pass validation.
	n, err := io.Copy(io.Discard, io.LimitReader(gz, (64<<10)+1))
	if n > 64<<10 {
		return ErrInvalid
	}
	return err
}

func validateAgentExecutable(ctx context.Context, binary, version, platform string) error {
	info, err := buildinfo.ReadFile(binary)
	if err != nil || info == nil || (info.Path != "dont/cmd/agent" && info.Path != "dont/agent/cmd/agent") {
		return fmt.Errorf("%w: package is not an Agent binary", ErrInvalid)
	}
	settings := map[string]string{}
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	if settings["GOOS"]+"-"+settings["GOARCH"] != platform {
		return ErrInvalid
	}
	var output limitedOutput
	command := exec.CommandContext(ctx, binary, "-build-info")
	command.Stdout, command.Stderr = &output, io.Discard
	if err := command.Run(); err != nil {
		return fmt.Errorf("Agent binary cannot run: %w", err)
	}
	var metadata struct {
		appbuild.Info
		Kind string `json:"kind"`
	}
	if json.Unmarshal(output.data, &metadata) != nil || metadata.Version != version || metadata.Kind != "agent" {
		return fmt.Errorf("%w: Agent binary version mismatch", ErrInvalid)
	}
	return nil
}

type limitedOutput struct{ data []byte }

func (b *limitedOutput) Write(data []byte) (int, error) {
	if len(b.data)+len(data) > 64<<10 {
		return 0, ErrInvalid
	}
	b.data = append(b.data, data...)
	return len(data), nil
}

func validateExecutable(ctx context.Context, binary, version, platform string) error {
	info, err := buildinfo.ReadFile(binary)
	if err != nil || info == nil || info.Path != "dont/cmd/admin-api" {
		return fmt.Errorf("%w: package is not a management binary", ErrInvalid)
	}
	settings := map[string]string{}
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	if settings["GOOS"]+"-"+settings["GOARCH"] != platform {
		return fmt.Errorf("%w: binary platform mismatch", ErrInvalid)
	}
	var output limitedOutput
	command := exec.CommandContext(ctx, binary, "-version")
	command.Stdout = &output
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return fmt.Errorf("management binary cannot run: %w", err)
	}
	var metadata struct {
		appbuild.Info
		EmbeddedWebUI bool `json:"embeddedWebUI"`
	}
	if err := json.Unmarshal(output.data, &metadata); err != nil {
		return err
	}
	if metadata.Version != version || !metadata.EmbeddedWebUI {
		return fmt.Errorf("%w: binary version or embedded frontend mismatch", ErrInvalid)
	}
	if !regexpFrontendCommit.MatchString(metadata.FrontendCommit) {
		return fmt.Errorf("%w: embedded frontend identity is missing", ErrInvalid)
	}
	// Native base archives have a different manifest. Dedicated update bundles
	// must agree with the frontend actually embedded in their manager.
	if data, err := regularRead(filepath.Join(filepath.Dir(binary), "manifest.json"), 64<<10); err == nil {
		var manifest Manifest
		if json.Unmarshal(data, &manifest) == nil && manifest.Protocol == Protocol && manifest.FrontendCommit != metadata.FrontendCommit {
			return fmt.Errorf("%w: embedded frontend identity mismatch", ErrInvalid)
		}
	}
	return nil
}

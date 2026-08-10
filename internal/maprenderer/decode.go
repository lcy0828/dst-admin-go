package maprenderer

import (
	"bufio"
	"bytes"
	"compress/flate"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	MaxInputSize      = int64(128 * 1024 * 1024)
	maxDecodedLuaSize = int64(256 * 1024 * 1024)
)

var ErrUnsupportedSaveFormat = errors.New("unsupported DST save format")

func DecodeSave(input io.Reader) ([]byte, error) {
	data, err := readLimited(input, MaxInputSize, "save input")
	if err != nil {
		return nil, err
	}
	data = bytes.TrimSpace(bytes.TrimRight(data, "\x00"))
	if bytes.HasPrefix(data, []byte("KLEI0001")) {
		return decodeCompressedSave(data)
	}
	return normalizeLuaSource(data)
}

func decodeCompressedSave(data []byte) ([]byte, error) {
	if len(data) < 11 {
		return nil, fmt.Errorf("%w: compressed header is incomplete", ErrUnsupportedSaveFormat)
	}
	payloadText := data[11:]
	compact := make([]byte, 0, len(payloadText))
	for _, value := range payloadText {
		switch value {
		case ' ', '\t', '\r', '\n':
		default:
			compact = append(compact, value)
		}
	}
	decoded := make([]byte, base64.StdEncoding.DecodedLen(len(compact)))
	count, err := base64.StdEncoding.Decode(decoded, compact)
	if err != nil {
		return nil, fmt.Errorf("decode compressed save payload: %w", err)
	}
	decoded = decoded[:count]
	if len(decoded) <= 16 {
		return nil, fmt.Errorf("%w: compressed payload is incomplete", ErrUnsupportedSaveFormat)
	}
	reader := flate.NewReader(bytes.NewReader(decoded[16:]))
	decompressed, readErr := readLimited(reader, maxDecodedLuaSize, "decompressed save")
	closeErr := reader.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close compressed save reader: %w", closeErr)
	}
	return normalizeLuaSource(decompressed)
}

func normalizeLuaSource(data []byte) ([]byte, error) {
	data = bytes.TrimSpace(bytes.TrimRight(data, "\x00"))
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: save is empty", ErrUnsupportedSaveFormat)
	}
	if index := luaStartIndex(data); index >= 0 {
		return append([]byte(nil), data[index:]...), nil
	}
	return nil, ErrUnsupportedSaveFormat
}

func luaStartIndex(data []byte) int {
	search := data
	if len(search) > 128 {
		search = search[:128]
	}
	for _, marker := range [][]byte{[]byte("local savedata"), []byte("return"), []byte("savedata=")} {
		if index := bytes.Index(search, marker); index >= 0 {
			return index
		}
	}
	return -1
}

func readLimited(reader io.Reader, limit int64, label string) ([]byte, error) {
	buffered := bufio.NewReader(io.LimitReader(reader, limit+1))
	data, err := io.ReadAll(buffered)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", label, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds %s", label, humanBytes(limit))
	}
	return data, nil
}

func humanBytes(value int64) string {
	if value%(1024*1024) == 0 {
		return fmt.Sprintf("%d MiB", value/(1024*1024))
	}
	return strings.TrimSpace(fmt.Sprintf("%d bytes", value))
}

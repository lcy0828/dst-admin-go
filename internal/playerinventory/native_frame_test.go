package playerinventory

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"
)

func TestNativePlayerSessionFrames(t *testing.T) {
	// Prefixes and lengths were captured from native engine SerializeUserSession
	// at the short/long header and byte boundaries. Only synthetic data is used.
	for _, test := range []struct {
		prefix string
		length int
	}{
		{"032200", 68}, {"032280", 69}, {"036200", 196},
		{"032181", 323}, {"032201", 324}, {"032281", 325},
		{"032204", 1092}, {"03a1c00000", 32835},
		{"03a2400000", 32836}, {"03a1000080", 65602},
		{"03a1800080", 65603}, {"03a2000080", 65604},
		{"03a2000100", 131140},
	} {
		t.Run(test.prefix, func(t *testing.T) {
			prefix, err := hex.DecodeString(test.prefix)
			if err != nil {
				t.Fatal(err)
			}
			body := append([]byte(`return {data={inventory={items={},equip={}}}}`), bytes.Repeat([]byte(" "), test.length-len(`return {data={inventory={items={},equip={}}}}`))...)
			for _, flag := range []byte{0, 1} {
				frame := append(append(append([]byte{}, prefix...), body...), flag)
				decoded, err := decode(frame)
				if err != nil || !bytes.Equal(decoded, body) {
					t.Fatalf("native length/flag decode: flag=%d length=%d err=%v", flag, test.length, err)
				}
				root, err := parse(context.Background(), frame)
				if err != nil || child(root, "data") == nil {
					t.Fatalf("native Lua payload did not parse: %v", err)
				}
				if _, err := decode(frame[:len(frame)-1]); err == nil {
					t.Fatal("accepted truncated frame")
				}
				// Actual player_classified records add binary minimap data here.
				// Its bytes (including a later Lua marker or invalid boolean byte)
				// must never extend or replace the length-delimited inventory body.
				extended := append(append([]byte{}, frame...), []byte("\x00\x10FEDCBA9876543210\xff\x02return os.execute(\"must-not-run\")\x00\xb8")...)
				decoded, err = decode(extended)
				if err != nil || !bytes.Equal(decoded, body) {
					t.Fatalf("classified suffix changed Lua payload: %v", err)
				}
				if _, err := parse(context.Background(), extended); err != nil {
					t.Fatalf("valid inventory with opaque suffix rejected: %v", err)
				}
				frame[len(frame)-1] = 2
				if _, err := decode(frame); err == nil {
					t.Fatal("accepted invalid session boolean")
				}
			}
		})
	}
}

func TestNativePlayerSessionDeclaredBoundaryIsAuthoritative(t *testing.T) {
	body := []byte(`return {data={inventory={items={},equip={}}}}`)
	// 15-bit string length encoding used by the native serializer.
	prefix := []byte{3, byte(len(body)&255) >> 1, byte(len(body)>>8) | byte(len(body)&1)<<7}
	frame := append(append(prefix, body...), 1)
	for _, data := range [][]byte{
		frame[:len(frame)-2], // truncated Lua body
		frame[:len(frame)-1], // missing envelope boolean
		append(append([]byte{}, frame[:len(frame)-1]...), 2, 0, 1), // invalid boundary boolean
	} {
		if _, err := parse(context.Background(), data); Code(err) != "SAVE_INVALID" {
			t.Fatalf("invalid declared boundary accepted: %v", err)
		}
	}
	// An unsupported expression inside the declared body remains forbidden,
	// regardless of whether a valid-looking table occurs later in the file.
	malicious := []byte(`return os.execute("must-not-run")`)
	bad := append([]byte{3, byte(len(malicious)) >> 1, byte(len(malicious)&1) << 7}, malicious...)
	bad = append(bad, 1)
	bad = append(bad, body...)
	if _, err := parse(context.Background(), bad); Code(err) != "SAVE_FORMAT_UNSUPPORTED" {
		t.Fatalf("suffix supplied a replacement root: %v", err)
	}
	oversize := append(append([]byte{}, frame...), make([]byte, MaxInput)...)
	if _, err := parse(context.Background(), oversize); Code(err) != "SAVE_LIMIT_EXCEEDED" {
		t.Fatalf("classified suffix bypassed input size limit: %v", err)
	}
}

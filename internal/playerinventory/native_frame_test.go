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
				if _, err := decode(append(frame, 1)); err == nil {
					t.Fatal("accepted trailing bytes")
				}
				frame[len(frame)-1] = 2
				if _, err := decode(frame); err == nil {
					t.Fatal("accepted invalid session boolean")
				}
			}
		})
	}
}

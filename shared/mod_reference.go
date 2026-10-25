package shared

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const LocalModCapability = "runtime.mods.local.v1"

// ModReference separates API identity from the exact DST configuration key.
// Workshop IDs retain their original numeric representation.
type ModReference struct {
	ID            string `json:"id"`
	Source        string `json:"source"`
	DirectoryName string `json:"directoryName"`
}

func ValidWorkshopID(id string) bool {
	if len(id) < 1 || len(id) > 20 {
		return false
	}
	for _, ch := range id {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

func ValidModDirectory(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 255 || !utf8.ValidString(name) || strings.ContainsAny(name, "/\\:") {
		return false
	}
	for _, ch := range name {
		if unicode.IsControl(ch) {
			return false
		}
	}
	return true
}

func ModReferenceFromKey(key string) (ModReference, bool) {
	if id, ok := strings.CutPrefix(key, "workshop-"); ok && ValidWorkshopID(id) {
		return ModReference{ID: id, Source: "workshop", DirectoryName: key}, true
	}
	if !ValidModDirectory(key) {
		return ModReference{}, false
	}
	return ModReference{ID: "local:" + key, Source: "local", DirectoryName: key}, true
}

func ParseModReference(id string) (ModReference, bool) {
	if ValidWorkshopID(id) {
		return ModReference{ID: id, Source: "workshop", DirectoryName: "workshop-" + id}, true
	}
	name, explicit := strings.CutPrefix(id, "local:")
	if !explicit {
		name = id
		// Bare numeric input, including oversized IDs, always means Workshop.
		allDigits := name != ""
		for _, ch := range name {
			allDigits = allDigits && ch >= '0' && ch <= '9'
		}
		if allDigits {
			return ModReference{}, false
		}
	}
	ref, ok := ModReferenceFromKey(name)
	if !ok || ref.Source != "local" {
		return ModReference{}, false
	}
	return ref, true
}

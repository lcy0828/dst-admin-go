package mods

import "dont/shared"

type ModReference = shared.ModReference

func ParseReference(id string) (ModReference, bool) { return shared.ParseModReference(id) }
func ValidReference(id string) bool                 { _, ok := ParseReference(id); return ok }
func referenceID(id string) string                  { ref, _ := ParseReference(id); return ref.ID }
func overrideKey(id string) string                  { ref, _ := ParseReference(id); return ref.DirectoryName }

func steamIdentity(id string) SteamMod {
	ref, _ := ParseReference(id)
	return SteamMod{ID: ref.ID, Source: ref.Source, DirectoryName: ref.DirectoryName}
}

func configuredReference(entry luaEntry) (ModReference, bool) {
	if entry.key == nil || entry.value == nil || entry.key.kind.String() != "string" || entry.value.kind.String() != "table" {
		return ModReference{}, false
	}
	ref, valid := shared.ModReferenceFromKey(entry.key.text)
	if valid && ref.Source == "local" && len(entry.value.entries) > 0 {
		_, enabled := entry.value.stringEntry("enabled")
		_, options := entry.value.stringEntry("configuration_options")
		if !enabled && !options {
			return ModReference{}, false
		}
	}
	return ref, valid
}

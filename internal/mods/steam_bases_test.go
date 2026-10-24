package mods

import "testing"

func TestNewSteamProviderWithBases(t *testing.T) {
	cases := []struct {
		name        string
		apiBase     string
		community   string
		wantAPI     string
		wantCommune string
	}{
		{"defaults", "", "", defaultSteamAPIBase, defaultSteamCommunityBase},
		{"blank-padded", "   ", "  ", defaultSteamAPIBase, defaultSteamCommunityBase},
		{"trim-trailing-slash", " http://api.steampowered.com/ ", "http://steamcommunity.com//", "http://api.steampowered.com", "http://steamcommunity.com"},
		{"custom", "http://127.0.0.1:8080", "http://127.0.0.1:8081", "http://127.0.0.1:8080", "http://127.0.0.1:8081"},
		{"custom-api-only", "https://mirror.example.com", "", "https://mirror.example.com", defaultSteamCommunityBase},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			provider := NewSteamProviderWithBases(" key ", "322330", testCase.apiBase, testCase.community)
			if provider.APIBase != testCase.wantAPI {
				t.Fatalf("APIBase = %q, want %q", provider.APIBase, testCase.wantAPI)
			}
			if provider.CommunityBase != testCase.wantCommune {
				t.Fatalf("CommunityBase = %q, want %q", provider.CommunityBase, testCase.wantCommune)
			}
			if provider.APIKey != "key" {
				t.Fatalf("APIKey = %q, want trimmed key", provider.APIKey)
			}
		})
	}
}

func TestNewSteamProviderKeepsDefaultBases(t *testing.T) {
	provider := NewSteamProvider("", "322330")
	if provider.APIBase != defaultSteamAPIBase || provider.CommunityBase != defaultSteamCommunityBase {
		t.Fatalf("defaults changed: api=%q community=%q", provider.APIBase, provider.CommunityBase)
	}
}

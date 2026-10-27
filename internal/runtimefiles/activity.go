package runtimefiles

import (
	"dont/shared"
	"errors"
	"fmt"
)

func activityArtifactNames() []string {
	names := []string{"activity-manifest.json"}
	for slot := 1; slot <= 64; slot++ {
		names = append(names, fmt.Sprintf("activity-%02d.json", slot))
	}
	return names
}

// RequestedArtifactNames confines incremental reads to generated activity files.
// No caller-supplied path, directory listing or arbitrary Runtime file reads.
func RequestedArtifactNames(kind shared.ArtifactKind, requested []string) ([]string, error) {
	allowed, ok := artifactNames[kind]
	if !ok {
		return nil, errors.New("unsupported artifact kind")
	}
	if len(requested) == 0 {
		if kind == shared.ArtifactPlayerActivity {
			return allowed[:1], nil
		}
		return allowed, nil
	}
	if kind != shared.ArtifactPlayerActivity || len(requested) > 8 {
		return nil, errors.New("invalid activity artifact selection")
	}
	seen := map[string]bool{}
	for _, name := range requested {
		found := false
		for _, candidate := range allowed {
			if candidate == name {
				found = true
				break
			}
		}
		if !found || seen[name] {
			return nil, errors.New("invalid activity artifact name")
		}
		seen[name] = true
	}
	return requested, nil
}

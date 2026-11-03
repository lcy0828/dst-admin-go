package worldmap

import (
	"encoding/json"
	"io"
	"math"

	"dont/internal/maprenderer"
)

// LocationSource validates an existing artifact. It never creates a map or
// treats a save-file digest as the stable world identity.
func (s *Service) LocationSource(room, world, id, session string, x, z float64) (string, error) {
	f, info, m, e := s.OpenArtifact(id, ArtifactManifest)
	if e != nil {
		return "", e
	}
	defer f.Close()
	if m.RoomID != room || m.WorldID != world || info.Size() > maxManifestSize {
		return "", ErrSessionNotFound
	}
	source, e := decodeSessionReference(m.SessionID)
	if e != nil || source.Session != session {
		return "", ErrSessionNotFound
	}
	var manifest maprenderer.Manifest
	if e = json.NewDecoder(io.LimitReader(f, maxManifestSize+1)).Decode(&manifest); e != nil {
		return "", e
	}
	b := manifest.Map.WorldBounds
	if math.IsNaN(x) || math.IsNaN(z) || x < b.MinX || x > b.MaxX || z < b.MinZ || z > b.MaxZ {
		return "", ErrRendererOutput
	}
	return m.ID + "@" + m.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"), nil
}

package intent

import (
	"context"
	"math"
	"sync"
)

// Embedder computes dense vector representations for query and capability texts.
// When nil or NoopEmbedder, no network or embedding work is performed.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// NoopEmbedder is the default zero-overhead embedder for single-host deployments.
type NoopEmbedder struct{}

func (NoopEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, nil
}

// CosineSimilarity computes the cosine similarity between two vectors.
func CosineSimilarity(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		normA += x * x
		normB += y * y
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// VectorCache lazily caches document vectors for a single catalog revision.
type VectorCache struct {
	mu       sync.Mutex
	revision string
	vectors  [][]float32
}

func (vc *VectorCache) GetOrBuild(ctx context.Context, embedder Embedder, revision string, docs []string) [][]float32 {
	if embedder == nil || len(docs) == 0 {
		return nil
	}
	vc.mu.Lock()
	defer vc.mu.Unlock()
	if vc.revision == revision && len(vc.vectors) == len(docs) {
		return vc.vectors
	}
	vecs, err := embedder.Embed(ctx, docs)
	if err != nil || len(vecs) != len(docs) {
		return nil
	}
	vc.revision = revision
	vc.vectors = vecs
	return vc.vectors
}

package intent

import "testing"

func TestBM25PreservesEnglishWordAndSchemaFieldBoundaries(t *testing.T) {
	index := NewBM25Index([]string{"List online players netScore performance", "Create isolated sample"})
	for _, q := range []string{"online players", "netScore", "performance"} {
		if index.Score(0, q) <= 0 || index.Score(1, q) != 0 {
			t.Fatal("lost token boundary", q)
		}
	}
}

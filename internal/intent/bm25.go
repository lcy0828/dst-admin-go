package intent

import (
	"math"
	"strings"
	"unicode"
)

// Tokenize extracts ASCII word tokens and Chinese character bigrams/trigrams
// for lightweight, dependency-free BM25 scoring.
func Tokenize(text string) []string {
	// Keep word boundaries. Compact would turn "online players" into one
	// token and join every English schema field in an indexed document.
	s := strings.ToLower(strings.TrimSpace(text))
	if s == "" {
		return nil
	}
	var tokens []string
	var hanBuf []rune
	var asciiBuf []rune

	flushHan := func() {
		n := len(hanBuf)
		if n == 1 {
			tokens = append(tokens, string(hanBuf))
		} else if n >= 2 {
			for i := 0; i+1 < n; i++ {
				tokens = append(tokens, string(hanBuf[i:i+2]))
			}
			for i := 0; i+2 < n; i++ {
				tokens = append(tokens, string(hanBuf[i:i+3]))
			}
		}
		hanBuf = hanBuf[:0]
	}

	flushASCII := func() {
		if len(asciiBuf) > 0 {
			w := string(asciiBuf)
			tokens = append(tokens, w)
			asciiBuf = asciiBuf[:0]
		}
	}

	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			flushASCII()
			hanBuf = append(hanBuf, r)
		} else if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '.' {
			flushHan()
			asciiBuf = append(asciiBuf, r)
		} else {
			flushHan()
			flushASCII()
		}
	}
	flushHan()
	flushASCII()
	return tokens
}

// BM25Index scores documents against queries using Okapi BM25.
type BM25Index struct {
	docs   [][]string
	tf     []map[string]int
	df     map[string]int
	avgLen float64
}

// NewBM25Index builds an in-memory BM25 index from document strings.
func NewBM25Index(documents []string) *BM25Index {
	idx := &BM25Index{
		docs: make([][]string, len(documents)),
		tf:   make([]map[string]int, len(documents)),
		df:   map[string]int{},
	}
	totalLen := 0
	for i, doc := range documents {
		toks := Tokenize(doc)
		idx.docs[i] = toks
		totalLen += len(toks)
		freq := map[string]int{}
		seen := map[string]bool{}
		for _, t := range toks {
			freq[t]++
			if !seen[t] {
				seen[t] = true
				idx.df[t]++
			}
		}
		idx.tf[i] = freq
	}
	if len(documents) > 0 {
		idx.avgLen = float64(totalLen) / float64(len(documents))
	}
	if idx.avgLen == 0 {
		idx.avgLen = 1
	}
	return idx
}

// Score returns the BM25 score of document docIdx for query.
func (idx *BM25Index) Score(docIdx int, query string) float64 {
	if idx == nil || docIdx < 0 || docIdx >= len(idx.docs) {
		return 0
	}
	qToks := Tokenize(query)
	if len(qToks) == 0 {
		return 0
	}
	const (
		k1 = 1.5
		b  = 0.75
	)
	N := float64(len(idx.docs))
	docLen := float64(len(idx.docs[docIdx]))
	freq := idx.tf[docIdx]
	score := 0.0
	seenQ := map[string]bool{}
	for _, qt := range qToks {
		if seenQ[qt] {
			continue
		}
		seenQ[qt] = true
		f := float64(freq[qt])
		if f == 0 {
			continue
		}
		df := float64(idx.df[qt])
		idf := math.Log(1.0 + (N-df+0.5)/(df+0.5))
		num := f * (k1 + 1.0)
		den := f + k1*(1.0-b+b*(docLen/idx.avgLen))
		score += idf * (num / den)
	}
	return score
}

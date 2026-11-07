package intent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"sync"

	"dont/internal/capreg"
)

// ActionDoc describes one enum sub-action of a capability for intent ranking.
type ActionDoc struct {
	ID          string
	Title       string
	Description string
	SearchTerms []string
	Parameters  map[string]string
}

// CapabilityDoc describes one capability for intent ranking and slot extraction.
type CapabilityDoc struct {
	ID             string
	Title          string
	Description    string
	Category       string
	Access         string
	Risk           string
	Tier           string
	Confirm        string
	SearchTerms    []string
	Aliases        []string
	Examples       []string
	Negatives      []string
	Prefer         string
	Parameters     map[string]any
	ParameterHints map[string]string
	SlotRoles      map[string]capreg.Slot
	Actions        []ActionDoc
	ActionPolicies map[string]capreg.Action
}

// Candidate is one ranked capability (or sub-action) returned by Service.Rank.
type Candidate struct {
	Capability     CapabilityDoc
	Action         *ActionDoc
	Score          int
	ExtractedSlots map[string]any
	Prefer         string
}

// Result contains the tagged query and ranked candidates.
type Result struct {
	Tagged     TaggedQuery
	Candidates []Candidate
}

// Service performs lexical + BM25 + entity/slot compatibility ranking with
// optional vector reranking.
type Service struct {
	Embedder  Embedder
	vectors   VectorCache
	lexicalMu sync.Mutex
	lexical   map[string]*BM25Index
}

func (s *Service) lexicalIndex(docs []string) *BM25Index {
	if s == nil {
		return NewBM25Index(docs)
	}
	h := sha256.New()
	for _, d := range docs {
		h.Write([]byte(d))
		h.Write([]byte{0})
	}
	key := hex.EncodeToString(h.Sum(nil))
	s.lexicalMu.Lock()
	defer s.lexicalMu.Unlock()
	if index := s.lexical[key]; index != nil {
		return index
	}
	if s.lexical == nil {
		s.lexical = map[string]*BM25Index{}
	}
	if len(s.lexical) >= 8 {
		for id := range s.lexical {
			delete(s.lexical, id)
			break
		}
	}
	index := NewBM25Index(docs)
	s.lexical[key] = index
	return index
}

// New creates a new intent understanding service.
func New(embedder Embedder) *Service {
	if embedder == nil {
		embedder = NoopEmbedder{}
	}
	return &Service{Embedder: embedder}
}

// Rank scores and orders allowed capabilities for rawQuery.
func (s *Service) Rank(ctx context.Context, rawQuery string, env Context, docs []CapabilityDoc, revision string) Result {
	tagged := TagEntities(rawQuery, env)
	qRaw := Compact(rawQuery)
	qNorm := tagged.Compact
	qTmpl := Compact(tagged.Template)

	bm25Docs := make([]string, len(docs))
	primaryDocs := make([]string, len(docs))
	for i, c := range docs {
		parts := []string{c.ID, c.Title}
		parts = append(parts, c.SearchTerms...)
		parts = append(parts, c.Aliases...)
		parts = append(parts, c.Examples...)
		for _, a := range c.Actions {
			parts = append(parts, a.ID, a.Title, a.Description)
			parts = append(parts, a.SearchTerms...)
		}
		primaryDocs[i] = strings.Join(parts, " ")
		bm25Docs[i] = primaryDocs[i] + " " + c.Description
	}
	bm25 := s.lexicalIndex(bm25Docs)
	primary := s.lexicalIndex(primaryDocs)

	var qVec []float32
	var docVecs [][]float32
	if s != nil && s.Embedder != nil {
		if vecs, err := s.Embedder.Embed(ctx, []string{tagged.Normalized}); err == nil && len(vecs) == 1 && len(vecs[0]) > 0 {
			qVec = vecs[0]
			docVecs = s.vectors.GetOrBuild(ctx, s.Embedder, revision, bm25Docs)
		}
	}

	var rows []Candidate
	for idx, c := range docs {
		if isNegated(qRaw, qNorm, c.Negatives) {
			continue
		}
		compatBonus := slotCompatibilityBonus(c, tagged)
		vecBonus := 0
		if len(qVec) > 0 && idx < len(docVecs) {
			if sim := CosineSimilarity(qVec, docVecs[idx]); sim > 0.65 {
				vecBonus = int((sim - 0.65) * 200)
			}
		}

		matchedAction := false
		actionNegated := false
		for _, action := range c.Actions {
			policy := c.ActionPolicies[action.ID]
			if isNegated(qRaw, qNorm, policy.Negatives) {
				actionNegated = true
				continue
			}
			terms := append(append([]string{action.ID, action.Title}, action.SearchTerms...), policy.Aliases...)
			pScore := max(
				phraseScore(qRaw, terms),
				max(phraseScore(qNorm, terms), phraseScore(qTmpl, terms)),
			)
			eScore := max(
				exampleScore(qRaw, policy.Examples),
				max(exampleScore(qNorm, policy.Examples), exampleScore(qTmpl, policy.Examples)),
			)
			base := max(pScore, eScore)
			if base > 0 {
				a := action
				prefer := policy.Prefer
				if prefer == "" {
					prefer = c.Prefer
				}
				slots := ExtractSlots(c, &a, tagged)
				rows = append(rows, Candidate{
					Capability:     c,
					Action:         &a,
					Score:          base + 100 + compatBonus + vecBonus,
					ExtractedSlots: slots,
					Prefer:         prefer,
				})
				matchedAction = true
			}
		}
		if matchedAction || actionNegated {
			continue
		}

		terms := append(append([]string{c.ID, c.Title}, c.SearchTerms...), c.Aliases...)
		pScore := max(
			phraseScore(qRaw, terms),
			max(phraseScore(qNorm, terms), phraseScore(qTmpl, terms)),
		)
		eScore := max(
			exampleScore(qRaw, c.Examples),
			max(exampleScore(qNorm, c.Examples), exampleScore(qTmpl, c.Examples)),
		)
		base := max(pScore, eScore)
		// API purpose/actions outrank incidental fields in a large response.
		// Schema-only matches can still surface newly documented metrics.
		bScore := primary.Score(idx, tagged.Normalized) + bm25.Score(idx, tagged.Normalized)*0.3
		total := 0
		if base > 0 {
			total = base + int(bScore*8) + compatBonus + vecBonus
		} else if compatBonus > 0 && bScore >= 2.0 {
			total = compatBonus + int(bScore*15) + vecBonus
		}
		// A useful schema/description match must still compete when another
		// document matches a generic word such as "player". The former global
		// fallback silently hid these candidates whenever any phrase matched.
		if bScore >= 6.0 {
			total = max(total, int(bScore*12)+vecBonus)
		}
		if total > 0 {
			rows = append(rows, Candidate{
				Capability:     c,
				Action:         nil,
				Score:          total,
				ExtractedSlots: ExtractSlots(c, nil, tagged),
				Prefer:         c.Prefer,
			})
		}
	}

	// Canonical preference demotion: a duplicate workbench capability that
	// declares Prefer: <canonical> never outranks the canonical capability when
	// both match.
	best := map[string]int{}
	for _, r := range rows {
		if r.Score > best[r.Capability.ID] {
			best[r.Capability.ID] = r.Score
		}
	}
	for i, r := range rows {
		if canonical, ok := best[r.Prefer]; ok && r.Prefer != "" && r.Prefer != r.Capability.ID && r.Score >= canonical {
			rows[i].Score = canonical - 1
		}
	}

	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].Score > rows[j].Score
	})

	return Result{
		Tagged:     tagged,
		Candidates: rows,
	}
}

// ExtractSlots populates schema-valid parameters extracted from TaggedQuery.
func ExtractSlots(c CapabilityDoc, action *ActionDoc, tagged TaggedQuery) map[string]any {
	out := map[string]any{}
	if action != nil {
		for k, v := range action.Parameters {
			out[k] = v
		}
	}
	props, _ := c.Parameters["properties"].(map[string]any)
	if len(props) == 0 {
		return out
	}
	for name := range props {
		slot := c.SlotRoles[name]
		hint := c.ParameterHints[name]
		switch {
		case (slot.Role == "target_player" || hint == "player") && tagged.Player != "":
			out[name] = tagged.Player
		case name == "query" && c.ID == "player.resolve" && tagged.Player != "":
			out[name] = tagged.Player
		case (slot.Role == "item" || name == "item" || name == "prefab") && tagged.Item != "":
			out[name] = tagged.Item
		case (slot.Role == "quantity" || name == "count") && tagged.Count > 0:
			out[name] = tagged.Count
		case name == "days" && tagged.Days > 0:
			out[name] = tagged.Days
		case name == "window" && tagged.Window != "":
			out[name] = tagged.Window
		case name == "enabled" && tagged.Enabled != nil:
			out[name] = *tagged.Enabled
		case name == "backup" && tagged.Backup != nil:
			out[name] = *tagged.Backup
		}
	}
	return out
}

func slotCompatibilityBonus(c CapabilityDoc, tagged TaggedQuery) int {
	bonus := 0
	props, _ := c.Parameters["properties"].(map[string]any)
	if tagged.Item != "" && tagged.Count > 0 {
		_, hasItem := props["item"]
		_, hasCount := props["count"]
		if hasItem && hasCount {
			bonus += 300
		}
	}
	if tagged.Days > 0 {
		if _, hasDays := props["days"]; hasDays {
			bonus += 300
		}
	}
	if tagged.HasPerfSignal && c.ID == "room.performance" {
		bonus += 300
	}
	if tagged.Backup != nil && c.ID == "room.reset" {
		bonus += 200
	}
	return bonus
}

func phraseScore(queryCompact string, terms []string) int {
	if queryCompact == "" {
		return 0
	}
	best := 0
	qLen := len([]rune(queryCompact))
	for _, rawTerm := range terms {
		term := Compact(rawTerm)
		if term == "" {
			continue
		}
		tLen := len([]rune(term))
		score := 0
		if term == queryCompact {
			score = 1000 + tLen
		} else if strings.Contains(queryCompact, term) {
			score = 100 + tLen
		} else if qLen >= 2 && strings.Contains(term, queryCompact) {
			score = qLen
		}
		if score > best {
			best = score
		}
	}
	return best
}

func exampleScore(queryCompact string, examples []string) int {
	if queryCompact == "" {
		return 0
	}
	best := 0
	for _, rawEx := range examples {
		ex := Compact(rawEx)
		if ex == "" {
			continue
		}
		eLen := len([]rune(ex))
		if ex == queryCompact {
			best = max(best, 1000+eLen)
		} else if strings.Contains(queryCompact, ex) {
			best = max(best, 100+eLen)
		}
	}
	return best
}

func isNegated(qRaw, qNorm string, negatives []string) bool {
	for _, neg := range negatives {
		n := Compact(neg)
		if n == "" {
			continue
		}
		if strings.Contains(qRaw, n) || strings.Contains(qNorm, n) {
			return true
		}
	}
	return false
}

package intent

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type corpusSingleTurn struct {
	ID               string         `json:"id"`
	Query            string         `json:"query"`
	WantCapability   string         `json:"want_capability"`
	WantTier         string         `json:"want_tier"`
	WantConfirm      string         `json:"want_confirm"`
	ForbidCapability string         `json:"forbid_capability"`
	Slots            map[string]any `json:"slots"`
}

func TestHeldOutSingleTurnCorpusAndFullRegistryRecall(t *testing.T) {
	docs := loadSnapshotDocs(t)
	svc := New(nil)

	f, err := os.Open(filepath.Join("testdata", "corpus", "single_turn.jsonl"))
	if err != nil {
		t.Fatalf("open single_turn.jsonl: %v", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var item corpusSingleTurn
		if err := json.Unmarshal(line, &item); err != nil {
			t.Fatalf("line %d: %v", lineNo, err)
		}
		res := svc.Rank(context.Background(), item.Query, Context{}, docs, "v1")
		if item.ForbidCapability != "" {
			for _, c := range res.Candidates[:min(len(res.Candidates), 3)] {
				if c.Capability.ID == item.ForbidCapability {
					t.Fatalf("[%s] query %q leaked into forbidden capability %s", item.ID, item.Query, item.ForbidCapability)
				}
			}
			continue
		}
		if len(res.Candidates) == 0 {
			t.Fatalf("[%s] query %q returned 0 candidates (expected %s)", item.ID, item.Query, item.WantCapability)
		}
		top := res.Candidates[0]
		if top.Capability.ID != item.WantCapability {
			t.Fatalf("[%s] query %q: top=%s, want=%s", item.ID, item.Query, top.Capability.ID, item.WantCapability)
		}
		if item.WantTier != "" && top.Capability.Tier != item.WantTier {
			t.Fatalf("[%s] query %q: tier=%s, want=%s", item.ID, item.Query, top.Capability.Tier, item.WantTier)
		}
		if item.WantConfirm != "" && top.Capability.Confirm != item.WantConfirm {
			t.Fatalf("[%s] query %q: confirm=%s, want=%s", item.ID, item.Query, top.Capability.Confirm, item.WantConfirm)
		}
		for k, wantV := range item.Slots {
			gotV := top.ExtractedSlots[k]
			// JSON numbers unmarshal as float64
			if wf, ok := wantV.(float64); ok {
				if gi, ok := gotV.(int); ok && float64(gi) == wf {
					continue
				}
			}
			if gotV != wantV {
				t.Fatalf("[%s] query %q slot %s: got %#v, want %#v", item.ID, item.Query, k, gotV, wantV)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}

	// Exhaustive recall across all 121 capabilities in the snapshot.
	totalUtterances := 0
	top1Hits := 0
	top3Hits := 0
	for _, doc := range docs {
		utterances := []string{doc.Title}
		utterances = append(utterances, doc.Aliases...)
		utterances = append(utterances, doc.Examples...)
		for _, u := range utterances {
			if u == "" {
				continue
			}
			totalUtterances++
			res := svc.Rank(context.Background(), u, Context{}, docs, "v1")
			if len(res.Candidates) == 0 {
				t.Fatalf("capability %s utterance %q returned 0 candidates", doc.ID, u)
			}
			expected := doc.ID
			if doc.Prefer != "" {
				expected = doc.Prefer
			}
			for rankIdx, cand := range res.Candidates[:min(len(res.Candidates), 3)] {
				if cand.Capability.ID == doc.ID || cand.Capability.ID == expected {
					top3Hits++
					if rankIdx == 0 {
						top1Hits++
					}
					break
				}
			}
		}
	}
	if totalUtterances < 600 {
		t.Fatalf("expected >= 600 registry utterances, got %d", totalUtterances)
	}
	top1Rate := float64(top1Hits) / float64(totalUtterances)
	if top1Rate < 0.97 {
		t.Fatalf("top-1 accuracy %.2f%% (%d/%d) below 97%% threshold", top1Rate*100, top1Hits, totalUtterances)
	}
}

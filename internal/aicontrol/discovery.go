package aicontrol

import (
	"context"
	"strings"

	"dont/internal/apicontract"
	"dont/internal/intent"
)

func nativeDiscoveryDocument(op *apicontract.Operation) intent.CapabilityDoc {
	semantic := op.Semantics()
	prefer := ""
	if op.Deprecated && op.ReplacedBy != "" {
		prefer = "api:" + op.ReplacedBy
	}
	return intent.CapabilityDoc{ID: "api:" + op.ID, Title: semantic.Purpose, Description: op.DiscoveryFields(), Access: op.ExecutionPolicy().Access,
		SearchTerms: append(append([]string{op.ID, op.Summary, op.Description}, semantic.Terms...), semantic.Actions...), Examples: semantic.Queries, Prefer: prefer}
}

// All active clients use one bounded on-demand index. Legacy transports retain
// their response envelopes and filters; no separately maintained vocabulary.
func (s *Service) rankDiscovery(ctx context.Context, query string, documents []intent.CapabilityDoc) intent.Result {
	s.searchOnce.Do(func() { s.search = intent.New(nil) })
	query = strings.TrimPrefix(strings.TrimSpace(query), "api_")
	for _, doc := range documents {
		if query == doc.ID || "api:"+query == doc.ID {
			return intent.Result{Candidates: []intent.Candidate{{Capability: doc, Score: 10000}}}
		}
	}
	if strings.TrimSpace(query) == "" {
		out := intent.Result{}
		for _, doc := range documents {
			out.Candidates = append(out.Candidates, intent.Candidate{Capability: doc, Score: 1})
		}
		return out
	}
	return s.search.Rank(ctx, strings.TrimPrefix(strings.TrimSpace(query), "api_"), intent.Context{}, documents, "native-v1")
}

func (s *Service) searchNative(p Principal, query string, filter apicontract.MetadataFilter) []*apicontract.Operation {
	docs := []intent.CapabilityDoc{}
	for _, op := range s.catalog.Operations() {
		if op.Callable() && filter.Matches(op.ToolMetadata()) && p.AllowsTool(op.ToolName()) {
			docs = append(docs, nativeDiscoveryDocument(op))
		}
	}
	out := []*apicontract.Operation{}
	for _, match := range s.rankDiscovery(context.Background(), query, docs).Candidates {
		if op, ok := s.catalog.Operation(strings.TrimPrefix(match.Capability.ID, "api:")); ok {
			out = append(out, op)
		}
	}
	return out
}

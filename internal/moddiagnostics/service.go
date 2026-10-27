package moddiagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"dont/internal/mods"
	"golang.org/x/sync/singleflight"
)

type BaselineStore interface {
	SaveOnce(Baseline) error
	Latest(string, string) (*Baseline, error)
}

type Service struct {
	source   *RuntimeSource
	store    BaselineStore
	requests singleflight.Group
	permits  chan struct{}
}

func NewService(source *RuntimeSource, store BaselineStore) *Service {
	return &Service{source: source, store: store, permits: make(chan struct{}, 2)}
}

func (s *Service) Analyze(ctx context.Context, roomID, worldID string) (Report, error) {
	if _, err := s.source.Catalog.World(roomID, worldID); err != nil {
		return Report{}, err
	}
	key, _ := json.Marshal([]string{roomID, worldID})
	// The bounded shared read outlives a disconnected browser. Every waiter
	// still respects its own cancellation; no output/side effect is replayed.
	result := s.requests.DoChan(string(key), func() (any, error) {
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		select {
		case s.permits <- struct{}{}:
			defer func() { <-s.permits }()
		case <-readCtx.Done():
			return Report{}, readCtx.Err()
		}
		scope := Scope{RoomID: roomID, WorldID: worldID}
		_, before, placementErr := s.source.Router.EndpointTarget(readCtx, roomID, worldID)
		if placementErr != nil {
			return s.report(readCtx, scope, nil, placementErr, true)
		}
		scope.TargetID, scope.InstallationID, scope.TopologyRevision = before.TargetID, before.InstallationID, before.TopologyRevision
		facts, factsErr := s.source.Mods.RoomCompatibilityFacts(readCtx, roomID)
		return s.report(readCtx, scope, &facts, factsErr, true)
	})
	select {
	case <-ctx.Done():
		return Report{}, ctx.Err()
	case out := <-result:
		if out.Err != nil {
			return Report{}, out.Err
		}
		return out.Val.(Report), nil
	}
}

// Preflight does not read game logs, contact Steam or execute any Lua.
// The caller bounds the batch to its update plan; facts are read once per room.
func (s *Service) Preflight(ctx context.Context, scopes []Scope) []Report {
	key, _ := json.Marshal(scopes)
	result := s.requests.DoChan("preflight:"+string(key), func() (any, error) {
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		select {
		case s.permits <- struct{}{}:
			defer func() { <-s.permits }()
		case <-readCtx.Done():
			return []Report{}, readCtx.Err()
		}
		return s.preflight(readCtx, scopes), nil
	})
	select {
	case out := <-result:
		if out.Err == nil {
			return out.Val.([]Report)
		}
	case <-ctx.Done():
	}
	return unavailableReports(scopes)
}

func unavailableReports(scopes []Scope) []Report {
	limited := len(scopes) > 128
	if limited {
		scopes = scopes[:128]
	}
	reports := []Report{}
	for _, scope := range scopes {
		reports = append(reports, Analyze(Input{Observation: Observation{RoomID: scope.RoomID, WorldID: scope.WorldID, Mods: []Mod{}, ObservedAt: time.Now().UTC(), Limitations: []string{"RUNTIME_UNAVAILABLE"}}}))
	}
	if limited && len(reports) > 0 {
		reports[len(reports)-1].Limitations = append(reports[len(reports)-1].Limitations, "PLAN_SCOPE_LIMIT_REACHED")
	}
	return reports
}

func (s *Service) preflight(ctx context.Context, scopes []Scope) []Report {
	limited := len(scopes) > 128
	if limited {
		scopes = scopes[:128]
	}
	facts := map[string]*mods.ModList{}
	errs := map[string]error{}
	reports := make([]Report, 0, len(scopes))
	for _, scope := range scopes {
		if _, seen := facts[scope.RoomID]; !seen {
			list, err := s.source.Mods.RoomCompatibilityFacts(ctx, scope.RoomID)
			facts[scope.RoomID], errs[scope.RoomID] = &list, err
		}
		r, err := s.report(ctx, scope, facts[scope.RoomID], errs[scope.RoomID], false)
		if err != nil {
			r = Analyze(Input{Observation: Observation{RoomID: scope.RoomID, WorldID: scope.WorldID, Mods: []Mod{}, ObservedAt: time.Now().UTC(), Limitations: []string{"RUNTIME_UNAVAILABLE"}}})
		}
		reports = append(reports, r)
	}
	if limited && len(reports) > 0 {
		last := &reports[len(reports)-1]
		last.Status = "insufficient_evidence"
		last.Limitations = append(last.Limitations, "PLAN_SCOPE_LIMIT_REACHED")
	}
	return reports
}

func (s *Service) Capture(ctx context.Context, releaseID string, scopes []Scope) error {
	var errs []error
	if len(scopes) > 128 {
		errs = append(errs, errors.New("mod diagnostic world limit reached"))
	}
	// A preview's versions are hints only. Capture reads the installed version
	// again immediately before execution, under the release's maintenance lock.
	captureScopes := append([]Scope(nil), scopes...)
	for index := range captureScopes {
		captureScopes[index].Version = Version{}
	}
	for _, report := range s.Preflight(ctx, captureScopes) {
		b := Baseline{ReleaseID: releaseID, CapturedAt: report.Observation.ObservedAt, Observation: report.Observation}
		if err := s.store.SaveOnce(b); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Service) report(ctx context.Context, scope Scope, facts *mods.ModList, factsErr error, logs bool) (Report, error) {
	in, err := s.source.Collect(ctx, scope, facts, factsErr, logs)
	if err != nil {
		return Report{}, err
	}
	baseline, err := s.store.Latest(scope.RoomID, scope.WorldID)
	if err != nil {
		in.Observation.Limitations = append(in.Observation.Limitations, "BASELINE_UNAVAILABLE")
	} else {
		in.Baseline = baseline
	}
	return Analyze(in), nil
}

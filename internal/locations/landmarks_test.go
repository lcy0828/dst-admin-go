package locations

import (
	"context"
	"dont/internal/landmarks"
	"dont/internal/worldquery"
	"encoding/json"
	"errors"
	"testing"
)

type landmarkQuery struct {
	result worldquery.Result
	err    error
	calls  []worldquery.Request
}

func (q *landmarkQuery) Query(_ context.Context, _ worldquery.Principal, in worldquery.Request) (worldquery.Result, error) {
	q.calls = append(q.calls, in)
	return q.result, q.err
}
func TestCommonDestinationsUseCurrentShardAndExistingTeleportChecks(t *testing.T) {
	for _, tc := range []struct {
		name               string
		points             []landmarks.Point
		ref                string
		incomplete, failed bool
		state, reason      string
	}{
		{name: "unique", points: []landmarks.Point{{ID: "pig_king:1", LandmarkID: "pig_king", X: 12, Z: 24}}, state: "prepared"},
		{name: "multiple", points: []landmarks.Point{{ID: "pig_king:1", LandmarkID: "pig_king", X: 12, Z: 24}, {ID: "pig_king:2", LandmarkID: "pig_king", X: 36, Z: 48}}, state: "needs_selection"},
		{name: "selected", points: []landmarks.Point{{ID: "pig_king:1", LandmarkID: "pig_king", X: 12, Z: 24}, {ID: "pig_king:2", LandmarkID: "pig_king", X: 36, Z: 48}}, ref: "pig_king:2", state: "prepared"},
		{name: "missing", state: "rejected", reason: "LANDMARK_NOT_FOUND"},
		{name: "stale choice", points: []landmarks.Point{{ID: "pig_king:1", LandmarkID: "pig_king", X: 12, Z: 24}}, ref: "pig_king:2", state: "rejected", reason: "LANDMARK_NOT_FOUND"},
		{name: "wrong kind", points: []landmarks.Point{{ID: "spawn:1", LandmarkID: "spawn", X: 12, Z: 24}}, state: "rejected", reason: "LANDMARK_NOT_FOUND"},
		{name: "incomplete", incomplete: true, state: "rejected", reason: "LANDMARK_SCAN_INCOMPLETE"},
		{name: "unavailable", failed: true, state: "rejected", reason: "LANDMARK_SCAN_INCOMPLETE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, rt := fixture(t)
			data, _ := json.Marshal(landmarks.Data{Items: tc.points, Count: len(tc.points)})
			q := &landmarkQuery{result: worldquery.Result{Status: "succeeded", Complete: !tc.incomplete, Data: data}}
			if tc.failed {
				q.err = errors.New("unavailable")
			}
			s.ConfigureLandmarks(q)
			p, err := s.Prepare(context.Background(), "actor", "room", PrepareInput{PlayerID: "KU_TEST", LocationName: "猪王村", LandmarkRef: tc.ref})
			if err != nil || p.State != tc.state || p.Reason != tc.reason {
				t.Fatalf("%+v %v", p, err)
			}
			if len(q.calls) != 1 || q.calls[0].WorldID != "world" || q.calls[0].QueryID != "landmarks" {
				t.Fatal(q.calls)
			}
			if rt.sent != 0 {
				t.Fatal("moved before confirmation")
			}
			if tc.state == "needs_selection" && len(p.Candidates) != 2 {
				t.Fatal(p)
			}
			if tc.state != "prepared" && len(rt.runs) != 0 {
				t.Fatal("invalid selection reached runtime")
			}
			if tc.state == "prepared" {
				if _, err := s.Execute(context.Background(), "actor", "room", p.ID); err != nil || rt.sent != 1 {
					t.Fatal(err, rt.sent)
				}
			}
		})
	}
}

func TestSavedNamesTakePriorityOverCommonDestinations(t *testing.T) {
	for _, name := range []string{"猪王", "猪王村", "出生点"} {
		for _, available := range []bool{false, true} {
			t.Run(name+"/"+map[bool]string{false: "missing", true: "present"}[available], func(t *testing.T) {
				s, rt := fixture(t)
				in := pointInput()
				in.Name = name
				saved, err := s.Save(context.Background(), "actor", "room", "", in)
				if err != nil {
					t.Fatal(err)
				}
				points := []landmarks.Point{}
				if available {
					points = append(points, landmarks.Point{ID: "pig_king:1", LandmarkID: "pig_king", X: 36, Z: 48})
				}
				data, _ := json.Marshal(landmarks.Data{Items: points, Count: len(points)})
				q := &landmarkQuery{result: worldquery.Result{Status: "succeeded", Complete: true, Data: data}}
				s.ConfigureLandmarks(q)
				p, err := s.Prepare(context.Background(), "actor", "room", PrepareInput{PlayerID: "KU_TEST", LocationName: name})
				if err != nil || p.State != "prepared" || p.LocationID != saved.ID || p.LocationName != name {
					t.Fatalf("saved name did not resolve: %+v %v", p, err)
				}
				if len(q.calls) != 0 || rt.sent != 0 {
					t.Fatal("saved name scanned common landmarks or moved before confirmation")
				}
			})
		}
	}
}

func TestSavedCommonNameCannotFallBackWhenWorldIsOld(t *testing.T) {
	s, rt := fixture(t)
	in := pointInput()
	in.Name = "猪王"
	saved, err := s.Save(context.Background(), "actor", "room", "", in)
	if err != nil {
		t.Fatal(err)
	}
	saved.SessionID = "previous-session"
	if _, err = s.store.Save(saved, saved.Revision); err != nil {
		t.Fatal(err)
	}
	q := &landmarkQuery{}
	s.ConfigureLandmarks(q)
	_, err = s.Prepare(context.Background(), "actor", "room", PrepareInput{PlayerID: "KU_TEST", LocationName: "猪王"})
	if !errors.Is(err, ErrOldWorld) || len(q.calls) != 0 || len(rt.runs) != 0 {
		t.Fatalf("old saved world must be rejected without fallback: %v %+v", err, q.calls)
	}
}

func TestExplicitLandmarkChoiceAndSavedIDStayDistinct(t *testing.T) {
	s, rt := fixture(t)
	in := pointInput()
	in.Name = "猪王"
	saved, err := s.Save(context.Background(), "saver", "room", "", in)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(landmarks.Data{Items: []landmarks.Point{{ID: "pig_king:1", LandmarkID: "pig_king", X: 12, Z: 24}}, Count: 1})
	q := &landmarkQuery{result: worldquery.Result{Status: "succeeded", Complete: true, Data: data}}
	s.ConfigureLandmarks(q)
	p, err := s.Prepare(context.Background(), "actor", "room", PrepareInput{PlayerID: "KU_TEST", LocationName: "猪王", LandmarkRef: "pig_king:1"})
	if err != nil || p.State != "prepared" || p.LocationID != "" || len(q.calls) != 1 {
		t.Fatalf("explicit common candidate was shadowed by saved name: %+v %v", p, err)
	}
	if _, err = s.Cancel(context.Background(), "actor", "room", p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Save(context.Background(), "actor", "room", saved.ID, SaveInput{Name: "改名基地", Revision: saved.Revision}); err != nil {
		t.Fatal(err)
	}
	p, err = s.Prepare(context.Background(), "actor", "room", PrepareInput{PlayerID: "KU_TEST", LocationID: saved.ID})
	if err != nil || p.State != "prepared" || p.LocationID != saved.ID || p.LocationName != "改名基地" || len(q.calls) != 1 || rt.sent != 0 {
		t.Fatalf("selected ID did not survive a rename: %+v %v", p, err)
	}
}

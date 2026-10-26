package httpapi

import (
	"dont/internal/agents"
	"github.com/gin-gonic/gin"
	"net/http/httptest"
	"testing"
)

func TestLocalModScopeIncludesOfflineCandidates(t *testing.T) {
	h := &ModHandler{}
	nodes := []agents.RuntimeTarget{{ID: "local", Online: true, Installations: []agents.RuntimeInstallation{{ID: "default"}}}}
	h.ConfigureRuntimeTargets(func() ([]agents.RuntimeTarget, error) { return nodes, nil })
	request := func(query string) (string, string, bool, int) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/mods/local:test"+query, nil)
		target, installation, ok := h.modDetailScope(c)
		return target, installation, ok, w.Code
	}
	target, installation, ok, _ := request("")
	if !ok || target != "local" || installation != "default" {
		t.Fatal(target, installation, ok)
	}
	nodes = append(nodes, agents.RuntimeTarget{ID: "agent:offline", Online: false, Installations: []agents.RuntimeInstallation{{ID: "default"}}})
	if _, _, ok, status := request(""); ok || status != 422 {
		t.Fatal("offline candidate was silently excluded")
	}
	target, installation, ok, _ = request("?targetId=agent:offline")
	if !ok || target != "agent:offline" || installation != "default" {
		t.Fatal("wrong inferred target", target, installation)
	}
	if _, _, ok, _ := request("?targetId=agent:missing"); ok {
		t.Fatal("unknown target fell back to local")
	}
}

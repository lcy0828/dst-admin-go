package console

import (
	"fmt"
	"strings"
	"testing"
)

func TestBeardActionsRespectNativeCharacterStages(t *testing.T) {
	for _, tc := range []struct {
		character string
		days      []int
	}{
		{"wilson", []int{0, 4, 8, 16}}, {"webber", []int{0, 3, 6, 9}},
	} {
		for level, action := range []string{"beard_none", "beard_short", "beard_medium", "beard_long"} {
			t.Run(tc.character+"/"+action, func(t *testing.T) {
				script, err := renderCharacterPower(map[string]interface{}{"player_id": "KU_EXAMPLE", "action": action})
				if err != nil {
					t.Fatal(err)
				}
				setup := fmt.Sprintf(`
local c={isgrowing=true,daysgrowth=16,daysgrowthaccumulator=.9,callbacks={}}
for _,d in ipairs({%d,%d,%d})do c.callbacks[d]=function()end end
function c:Reset()self.daysgrowth=0;self.daysgrowthaccumulator=0 end
function c:SetSkin()assert(type(self.callbacks[self.daysgrowth])=="function")end
function c:UpdateBeardInventory()end
p={userid="KU_EXAMPLE",prefab=%q,components={beard=c},HasTag=function()return false end};AllPlayers={p}
`, tc.days[1], tc.days[2], tc.days[3], tc.character)
				out, err := auditLuaOutput(t, script+fmt.Sprintf(`;assert(p.components.beard.daysgrowth==%d);assert(p.components.beard.daysgrowthaccumulator==0)`, tc.days[level]), setup)
				if err != nil || !strings.Contains(out, "true") {
					t.Fatal(out, err)
				}
			})
		}
	}
}

func TestBeardActionsRejectWrongCharacterGhostAndMissingNativeCallback(t *testing.T) {
	script, _ := renderCharacterPower(map[string]interface{}{"player_id": "KU_EXAMPLE", "action": "beard_long"})
	for _, scenario := range []string{
		`p.prefab="wendy"`, `p.HasTag=function()return true end`, `p.components.beard.callbacks={}`,
		`p.components.beard.isgrowing=false`, `p.components.beard=nil`,
	} {
		setup := `local c={isgrowing=true,daysgrowth=0,callbacks={[9]=function()end}};function c:SetSkin()error("unexpected mutation")end;function c:UpdateBeardInventory()error("unexpected mutation")end;p={userid="KU_EXAMPLE",prefab="webber",components={beard=c},HasTag=function()return false end};AllPlayers={p};` + scenario
		out, err := auditLuaOutput(t, script, setup)
		if err != nil || !strings.Contains(out, "false") {
			t.Fatal(scenario, out, err)
		}
	}
}

package console

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Existing callers omitting phase keep their trusted program contract. Current
// interactive clients opt into one-use, frozen multi-player previews.
func addMovementPreviews(templates map[string]template) {
	for _, id := range []string{"teleport_player", "relocate_players", "teleport_to_nearest_entity"} {
		t := templates[id]
		render := t.render
		t.definition.Parameters = append(t.definition.Parameters,
			Parameter{Name: "phase", Label: "传送步骤", Type: "enum", Options: []string{"preview", "execute", "cancel"}},
			Parameter{Name: "preview_id", Label: "传送确认编号", Type: "string"})
		t.render = func(args map[string]interface{}) (string, error) {
			value, present := args["phase"]
			phase, typed := value.(string)
			if !present {
				return render(args)
			}
			if !typed || phase == "" {
				return "", ErrInvalidArguments
			}
			if phase != "preview" && phase != "execute" && phase != "cancel" {
				return "", ErrInvalidArguments
			}
			id := uuid.NewString()
			if phase != "preview" {
				var ok bool
				id, ok = args["preview_id"].(string)
				if _, err := uuid.Parse(id); !ok || err != nil {
					return "", ErrInvalidArguments
				}
			}
			options := fmt.Sprintf(`{phase=%s,previewId=%s}`, quoteLua(phase), quoteLua(id))
			bridge := `local w=DSTAdmin and DSTAdmin.Commands and DSTAdmin.Commands.Workbench;if not w or not w.MovePlayers then error("WORKBENCH_UPDATE_REQUIRED")end;`
			if phase != "preview" {
				return bridge + `w.MovePlayers(nil,` + options + `)`, nil
			}
			script, err := render(args)
			if err != nil {
				return "", err
			}
			return `local function dst_move(moves)` + bridge + `return w.MovePlayers(moves,` + options + `)end;` + strings.ReplaceAll(script, "w.MovePlayers(", "dst_move("), nil
		}
		templates[id] = t
	}
}

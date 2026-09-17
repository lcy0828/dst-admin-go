package dstruntime

import (
	"crypto/sha256"
	"encoding/hex"

	"dont/shared"
)

const maximumDirectRuntimeCommandBytes = 1000

func commandDelivery(request CommandRequest, payload []byte) (string, *shared.RuntimeCommandDocument) {
	direct := `DSTAdmin.Commands.ExecuteJSON(` + quoteRuntimeLua(string(payload)) + `)`
	if len(direct) <= maximumDirectRuntimeCommandBytes {
		return direct, nil
	}
	sum := sha256.Sum256(payload)
	document := &shared.RuntimeCommandDocument{
		RequestID: request.RequestID,
		SHA256:    hex.EncodeToString(sum[:]),
		Data:      append([]byte(nil), payload...),
	}
	trigger := `DSTAdmin.Commands.ExecuteFile(` + quoteRuntimeLua(request.RequestID) + `,` + quoteRuntimeLua(request.Action) + `)`
	return trigger, document
}

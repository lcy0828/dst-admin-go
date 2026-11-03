package runtimedriver

import (
	"context"
	"errors"
	"testing"

	"dont/internal/agents"
	"dont/shared"
)

type downloadReceiptExecutor struct{ scenario string }

func (*downloadReceiptExecutor) ExecuteShard(context.Context, string, shared.ShardOperationRequest, int) (agents.ShardExecutionResult, error) {
	return agents.ShardExecutionResult{}, nil
}
func (e *downloadReceiptExecutor) ExecuteRuntime(_ context.Context, _ string, request shared.RuntimeOperationRequest, _ int) (agents.RuntimeExecutionResult, error) {
	result := shared.RuntimeOperationResult{ProtocolVersion: request.ProtocolVersion, OperationID: request.OperationID, InstallationID: request.InstallationID, Action: request.Action, Outcome: shared.RuntimeOutcomeConfirmed, Mod: &shared.RuntimeModResult{Complete: true}}
	var err error
	switch e.scenario {
	case "timeout":
		err = context.DeadlineExceeded
		result = shared.RuntimeOperationResult{}
	case "sent":
		result.Outcome = shared.RuntimeOutcomeSent
	case "missing":
		result.Mod = nil
	case "incomplete":
		result.Mod.Complete = false
	case "failed", "wrong-operation":
		result.Outcome = shared.RuntimeOutcomeFailed
		err = errors.New("SteamCMD failed")
		if e.scenario == "wrong-operation" {
			result.OperationID = "another-operation"
		}
	}
	return agents.RuntimeExecutionResult{Result: result}, err
}
func TestDownloadDistinguishesFailureFromMissingOrUnrelatedReceipts(t *testing.T) {
	for _, scenario := range []string{"complete", "sent", "failed", "wrong-operation", "timeout", "missing", "incomplete"} {
		t.Run(scenario, func(t *testing.T) {
			driver, err := NewAgent(&downloadReceiptExecutor{scenario: scenario})
			if err != nil {
				t.Fatal(err)
			}
			err = driver.DownloadMods(context.Background(), Target{TargetID: "agent:test", InstallationID: "default"}, Operation{ID: "download"}, []string{"100"})
			if scenario == "complete" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			uncertain := scenario != "failed"
			if err == nil || errors.Is(err, ErrModDownloadUncertain) != uncertain {
				t.Fatal(err)
			}
		})
	}
}

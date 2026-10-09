package shared

import "testing"

func TestAgentSoftwareCommandContract(t *testing.T) {
	base := AgentUpgradeRequest{ProtocolVersion: AgentUpgradeProtocolVersion, ReleaseID: "1234567890abcdef1234567890abcdef", OS: "windows", Arch: "amd64", Action: "status"}
	if err := ValidateAgentSoftwareRequest(base); err != nil {
		t.Fatal(err)
	}
	rollback := base
	rollback.Action, rollback.Version = "rollback", "v1.2.2"
	if ValidateAgentSoftwareRequest(rollback) == nil {
		t.Fatal("manual downgrade accepted")
	}
	for _, change := range []func(*AgentUpgradeRequest){func(r *AgentUpgradeRequest) { r.Action = "exec" }, func(r *AgentUpgradeRequest) { r.DownloadPath = "https://example.com/program.exe" }, func(r *AgentUpgradeRequest) { r.Source = "untrusted" }, func(r *AgentUpgradeRequest) { r.Action = "apply" }, func(r *AgentUpgradeRequest) { r.Action = "update"; r.Version = "v1.2.3; rm -rf /" }} {
		request := base
		change(&request)
		if ValidateAgentSoftwareRequest(request) == nil {
			t.Fatal("unsafe request accepted", request)
		}
	}
	base.Action, base.Version, base.Source = "update", "v1.2.3", "controller"
	base.DownloadPath = "/agent-software-updates/" + base.ReleaseID
	base.DownloadToken = "short-lived-valid-token"
	base.SHA256 = "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
	base.Size = 123
	if err := ValidateAgentSoftwareRequest(base); err != nil {
		t.Fatal(err)
	}
}

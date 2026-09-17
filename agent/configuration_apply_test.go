package agent

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"dont/shared"
)

func TestInlineConfigurationApplyIsFencedIdempotentAndReportsConflict(t *testing.T) {
	runtime := &fakeShardRuntime{}
	agent, installation := newShardOperationAgent(t, runtime)
	root := filepath.Join(installation.SavePath, "Cluster_1")
	if err := os.MkdirAll(filepath.Join(root, "Master"), 0750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "cluster.ini")
	before, after := []byte("[NETWORK]\ncluster_name=before\n"), []byte("[NETWORK]\ncluster_name=after\n")
	if err := os.WriteFile(path, before, 0640); err != nil {
		t.Fatal(err)
	}
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	entry, err := archive.Create("cluster.ini")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write(after); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	request := runtimeOperationRequest(shared.RuntimeActionConfigurationApply)
	request.Configuration = &shared.RuntimeConfigurationRequest{
		PublicationID: "inline-publication-1", Scope: "shared", Data: buffer.Bytes(), Size: int64(buffer.Len()),
		SHA256: fmt.Sprintf("%x", sha256.Sum256(buffer.Bytes())), ExpectedFiles: map[string]string{"cluster.ini": fmt.Sprintf("%x", sha256.Sum256(before))},
	}
	invalid := request
	invalid.LeaseID = ""
	if _, err := agent.executeRuntimeOperation(string(invalid.Action), &invalid, 10); err == nil {
		t.Fatal("inline write bypassed mutation lease")
	}
	result, err := agent.executeRuntimeOperation(string(request.Action), &request, 10)
	if err != nil || result.Configuration == nil || !result.Configuration.Complete {
		t.Fatalf("inline save=%#v, %v", result, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, after) {
		t.Fatalf("saved data=%q, %v", data, err)
	}
	replayed, err := agent.executeRuntimeOperation(string(request.Action), &request, 10)
	if err != nil || !replayed.Idempotent {
		t.Fatalf("duplicate save=%#v, %v", replayed, err)
	}
	request.OperationID, request.OperationKey = "changed-config-1", "changed-config-key-1"
	request.Configuration.PublicationID = "inline-publication-2"
	result, err = agent.executeRuntimeOperation(string(request.Action), &request, 10)
	if err == nil || result.Configuration == nil || !result.Configuration.RevisionConflict || result.Configuration.Complete {
		t.Fatalf("conflict not preserved: %#v, %v", result, err)
	}
	if len(runtime.calls) != 0 {
		t.Fatalf("configuration save touched running world: %v", runtime.calls)
	}
}

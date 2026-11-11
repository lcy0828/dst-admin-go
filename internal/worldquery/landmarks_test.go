package worldquery

import (
 "fmt"
 "testing"
 "dont/internal/dstruntime"
)
func TestLandmarkOutputRequiresCompleteBoundedStructuredEvidence(t *testing.T){
 for _,tc:=range []struct{data string;valid bool}{
  {`{"items":[{"id":"pig_king:1","landmarkId":"pig_king","x":12,"z":24}],"count":1}`,true},
  {`{"items":[],"count":0}`,true},
  {`{"items":{},"count":0}`,true},
  {`{"items":[{"id":"pig_king:1","landmarkId":"pig_king","z":24}],"count":1}`,false},
  {`{"items":[{"id":"pig_king:1","landmarkId":"pig_king","x":100001,"z":24}],"count":1}`,false},
  {`{"items":[{"id":"x","landmarkId":"arbitrary_prefab","x":12,"z":24}],"count":1}`,false},
  {`{"items":[],"count":1}`,false},
  {`{"items":null,"count":0}`,false},
 }{
  text:=fmt.Sprintf(`{"protocolVersion":1,"runId":"test-query","queryId":"landmarks","complete":true,"data":%s,"scannedEntities":100}`,tc.data)
  receipt:=dstruntime.CommandReceipt{OK:true,Details:map[string]interface{}{"output":map[string]interface{}{"text":text,"truncated":false}}}
  result:=acceptReceipt(Result{QueryRunID:"test-query"},Request{QueryID:"landmarks"},receipt)
  if (result.Status=="succeeded"&&result.Complete)!=tc.valid{t.Fatal(tc.data,result)}
 }
}

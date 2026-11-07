package apicontract

import (
	"strings"
	"testing"
)

func TestDiscoveryFieldsFindsNewReadDataWithoutPromotingMutationResponses(t *testing.T) {
	c, err := Parse([]byte(`openapi: 3.1.0
paths:
  /sample:
    get:
      operationId: inspectSample
      summary: 查看样本
      responses:
        '200': {description: Result, content: {application/json: {schema: {$ref: '#/components/schemas/Sample'}}}}
    delete:
      operationId: removeSample
      summary: 删除样本
      responses:
        '200': {description: Result, content: {application/json: {schema: {$ref: '#/components/schemas/Sample'}}}}
components:
  schemas:
    Sample:
      type: object
      properties:
        canopyIndex: {type: number, description: 合成树冠指数}
        recursive: {$ref: '#/components/schemas/Sample'}
`))
	if err != nil {
		t.Fatal(err)
	}
	read, _ := c.Operation("inspectSample")
	write, _ := c.Operation("removeSample")
	text := read.DiscoveryFields()
	if !strings.Contains(text, "合成树冠指数") || !strings.Contains(text, "canopyIndex") || len(text) > 8192 {
		t.Fatal("documented read data is not discoverable or bounded", text)
	}
	if strings.Contains(write.DiscoveryFields(), "canopyIndex") {
		t.Fatal("mutation promoted as data query")
	}
	if text != read.DiscoveryFields() {
		t.Fatal("cached projection changed")
	}
	fields := read.MatchingFields([]string{"树冠", "指数"})
	if len(fields) != 1 || !strings.Contains(fields[0], "canopyIndex") {
		t.Fatal("model cannot see why the returned schema matched", fields)
	}
}

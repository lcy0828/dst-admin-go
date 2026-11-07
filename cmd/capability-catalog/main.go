// capability-catalog generates discovery documentation from native contracts.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"dont/internal/apicontract"
)

func main() {
	semantics := flag.String("semantics", "", "write the legacy Chinese discovery projection")
	check := flag.Bool("check", false, "check the projection instead of writing it")
	flag.Parse()
	c, err := apicontract.Default()
	if err != nil {
		fail(err)
	}
	entries := map[string]*apicontract.ToolSemantics{}
	coverage := []map[string]any{}
	for _, op := range c.Operations() {
		reason := ""
		if !op.Callable() {
			reason = "identity/model transport; intentionally not a business tool"
		} else if strings.Contains(op.Path, "/bot/") {
			reason = "legacy task facade; original endpoints remain available"
		}
		coverage = append(coverage, map[string]any{"operationId": op.ID, "genericCallable": reason == "", "reason": reason, "scope": op.Scope(), "policy": op.ExecutionPolicy(), "transport": op.Transport})
		if op.Callable() {
			entries[op.ID] = op.Semantics()
		}
	}
	if *semantics != "" {
		data, e := json.MarshalIndent(map[string]any{"schemaVersion": 1, "locale": "zh-CN", "operations": entries}, "", "  ")
		if e != nil {
			fail(e)
		}
		data = append(data, '\n')
		if *check {
			old, e := os.ReadFile(*semantics)
			if e != nil || !bytes.Equal(old, data) {
				fail(fmt.Errorf("generated projection is stale: %s", *semantics))
			}
		} else if e = os.WriteFile(*semantics, data, 0644); e != nil {
			fail(e)
		}
		return
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err = encoder.Encode(map[string]any{"operations": coverage, "note": "Discovery/invocation coverage only; this is not end-to-end game or QQ acceptance."}); err != nil {
		fail(err)
	}
}
func fail(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }

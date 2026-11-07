package aicontrol

import (
	"dont/internal/apicontract"
	"testing"
)

// Includes native document construction and authorization filtering on each
// request; the lexical index is the only warmed cache. No external embedder.
func BenchmarkNativeDiscovery(b *testing.B) {
	catalog, err := apicontract.Default()
	if err != nil {
		b.Fatal(err)
	}
	s := &Service{catalog: catalog}
	queries := []string{"复活玩家", "最近一天 CPU 内存 性能", "玩家网络档位 netScore", "地图", "模组配置"}
	for _, q := range queries {
		s.searchNative(Principal{}, q, apicontract.MetadataFilter{})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.searchNative(Principal{}, queries[i%len(queries)], apicontract.MetadataFilter{})
	}
}

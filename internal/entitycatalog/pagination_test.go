package entitycatalog

import (
	"fmt"
	"testing"
)

func TestPageEntitiesFiltersBeforePaginationAndDoesNotChangeCache(t *testing.T) {
	items := make([]Entity, 22000)
	for i := range items {
		items[i] = Entity{ID: fmt.Sprintf("prefab_%05d", i), NameZhCN: "物品", NameEn: "Item"}
	}
	page, total, err := PageEntities(items, PageOptions{Limit: 60, Offset: 60})
	if err != nil || len(page) != 60 || total != 22000 || page[0].ID != "prefab_00060" || items[0].ID != "prefab_00000" {
		t.Fatal(page, total, err)
	}
	items[100].NameZhCN, items[300].NameEn = "测试物品", "Test Item"
	page, total, err = PageEntities(items, PageOptions{Query: "test", Limit: 1})
	if err != nil || total != 1 || len(page) != 1 || page[0].ID != "prefab_00300" {
		t.Fatal(page, total, err)
	}
	page, total, err = PageEntities(items, PageOptions{Query: "测试", Limit: 1, Offset: 1})
	if err != nil || total != 1 || len(page) != 0 {
		t.Fatal(page, total, err)
	}
	page, total, err = PageEntities(items, PageOptions{Limit: 60, Complete: true})
	if err != nil || total != 22000 || len(page) != total {
		t.Fatal(len(page), total, err)
	}
}

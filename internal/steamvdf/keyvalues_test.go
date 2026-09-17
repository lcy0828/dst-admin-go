package steamvdf

import (
	"reflect"
	"testing"
)

func TestMarshalRoundTrip(t *testing.T) {
	want := map[string]interface{}{
		"AppWorkshop": map[string]interface{}{
			"appid": "322330",
			"WorkshopItemsInstalled": map[string]interface{}{
				"3687959533": map[string]interface{}{
					"manifest": "5198458395439210153",
					"size":     "45090",
				},
			},
		},
	}
	encoded, err := Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch:\n got=%#v\nwant=%#v", got, want)
	}
}

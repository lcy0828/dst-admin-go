package configuration

import (
	"errors"
	"testing"
)

func TestRoomConfigurationRejectsUnspecifiedMasterDestination(t *testing.T) {
	service, _, _ := newConfigurationService(t)
	current, err := service.RoomConfig("room")
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"0.0.0.0", "::", "0:0:0:0:0:0:0:0", "::ffff:0.0.0.0", " 0.0.0.0 "} {
		t.Run(address, func(t *testing.T) {
			values := current.Values
			values.ShardEnabled = true
			values.BindIP = "0.0.0.0"
			values.MasterIP = address
			_, err := service.PreviewRoom("room", RoomUpdateRequest{ExpectedRevision: current.Revision, Values: values})
			var fields *FieldError
			if !errors.As(err, &fields) || fields.Fields["masterIp"] == "" {
				t.Fatalf("unspecified connection destination accepted: %v", err)
			}
		})
	}
	for _, address := range []string{"127.0.0.1", "::1", "192.0.2.10", "master.example.test"} {
		values := current.Values
		values.ShardEnabled = true
		values.BindIP = "0.0.0.0"
		values.MasterIP = address
		if _, err := service.PreviewRoom("room", RoomUpdateRequest{ExpectedRevision: current.Revision, Values: values}); err != nil {
			t.Fatalf("valid connection destination %s rejected: %v", address, err)
		}
	}
}

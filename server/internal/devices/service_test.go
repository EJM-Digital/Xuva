package devices

import (
	"context"
	"testing"

	"github.com/EJM-Digital/Xuva/server/internal/database"
)

func TestApprovedDevicesPersistAcrossReload(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	service := NewPersistentService(db)
	device, err := service.Approve(context.Background(), ApproveInput{
		DeviceID:      "device_living_room",
		DeviceName:    "Living Room Apple TV",
		ClientProfile: "apple-tv",
		ApprovedBy:    "owner",
	})
	if err != nil {
		t.Fatalf("approve device: %v", err)
	}
	if device.ID == "" || device.Status != StatusApproved {
		t.Fatalf("expected approved device record, got %#v", device)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}

	reopened, err := database.Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("reopen database: %v", err)
	}
	defer reopened.Close()
	reloaded := NewPersistentService(reopened)
	items, err := reloaded.ListApproved(context.Background())
	if err != nil {
		t.Fatalf("list approved devices: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected one approved device after reload, got %#v", items)
	}
	if items[0].DisplayName != "Living Room Apple TV" || items[0].ClientProfile != "apple-tv" {
		t.Fatalf("expected persisted approved device details, got %#v", items[0])
	}
}

func TestApproveDuplicateDeviceIDUpdatesExistingRecord(t *testing.T) {
	db, err := database.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer db.Close()
	service := NewPersistentService(db)

	first, err := service.Approve(context.Background(), ApproveInput{
		DeviceID:      "device_family_room",
		DeviceName:    "Family Room Apple TV",
		ClientProfile: "apple-tv",
		ApprovedBy:    "owner",
	})
	if err != nil {
		t.Fatalf("approve first device: %v", err)
	}
	second, err := service.Approve(context.Background(), ApproveInput{
		DeviceID:      "device_family_room",
		DeviceName:    "Family Room Apple TV 4K",
		ClientProfile: "apple-tv",
		ApprovedBy:    "owner",
	})
	if err != nil {
		t.Fatalf("approve duplicate device: %v", err)
	}
	items, err := service.ListApproved(context.Background())
	if err != nil {
		t.Fatalf("list approved devices: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected one approved device after duplicate update, got %#v", items)
	}
	if first.ID != second.ID || items[0].DisplayName != "Family Room Apple TV 4K" {
		t.Fatalf("expected duplicate device update to reuse record, got first=%#v second=%#v items=%#v", first, second, items)
	}
}

func TestRevokedDevicesDisappearFromApprovedList(t *testing.T) {
	db, err := database.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer db.Close()
	service := NewPersistentService(db)

	device, err := service.Approve(context.Background(), ApproveInput{
		DeviceID:      "device_guest_room",
		DeviceName:    "Guest Room Tablet",
		ClientProfile: "ios",
		ApprovedBy:    "owner",
	})
	if err != nil {
		t.Fatalf("approve device: %v", err)
	}
	if _, err := service.Revoke(context.Background(), device.ID); err != nil {
		t.Fatalf("revoke device: %v", err)
	}
	items, err := service.ListApproved(context.Background())
	if err != nil {
		t.Fatalf("list approved devices: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected no approved devices after revoke, got %#v", items)
	}
}

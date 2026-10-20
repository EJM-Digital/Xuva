package pairing

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/database"
)

func TestCreateApproveAndHideCodeAfterApproval(t *testing.T) {
	service := NewService()
	request, err := service.Create(CreateRequest{DeviceName: "Living Room", ClientProfile: "apple-tv"})
	if err != nil {
		t.Fatalf("create pairing: %v", err)
	}
	if request.ID == "" || len(request.Code) != 6 || request.Status != StatusPending {
		t.Fatalf("expected pending request with six digit code, got %#v", request)
	}

	approved, err := service.Approve(request.ID, "admin")
	if err != nil {
		t.Fatalf("approve pairing: %v", err)
	}
	if approved.Status != StatusApproved || approved.DeviceID == "" || approved.Code != "" {
		t.Fatalf("expected approved request with device id and hidden code, got %#v", approved)
	}
}

func TestCannotApproveAlreadyApprovedRequest(t *testing.T) {
	service := NewService()
	request, err := service.Create(CreateRequest{})
	if err != nil {
		t.Fatalf("create pairing: %v", err)
	}
	if _, err := service.Approve(request.ID, "admin"); err != nil {
		t.Fatalf("first approve: %v", err)
	}
	if _, err := service.Approve(request.ID, "admin"); err != ErrClosed {
		t.Fatalf("expected ErrClosed on double-approve, got %v", err)
	}
}

func TestDenyDeletesRequestImmediately(t *testing.T) {
	service := NewService()
	request, err := service.Create(CreateRequest{})
	if err != nil {
		t.Fatalf("create pairing: %v", err)
	}
	if _, err := service.Deny(request.ID, "admin"); err != nil {
		t.Fatalf("deny pairing: %v", err)
	}
	if _, ok := service.Get(request.ID); ok {
		t.Fatal("expected denied request to be gone, but Get still returned it")
	}
	if _, err := service.Approve(request.ID, "admin"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound after deny, got %v", err)
	}
}

func TestListOnlyReturnsActivePendingRequests(t *testing.T) {
	service := NewService()
	pending, err := service.Create(CreateRequest{DeviceName: "Pending TV"})
	if err != nil {
		t.Fatalf("create pending pairing: %v", err)
	}
	approved, err := service.Create(CreateRequest{DeviceName: "Approved TV"})
	if err != nil {
		t.Fatalf("create approved pairing: %v", err)
	}
	if _, err := service.Approve(approved.ID, "admin"); err != nil {
		t.Fatalf("approve pairing: %v", err)
	}

	items := service.List()
	if len(items) != 1 || items[0].ID != pending.ID || items[0].Status != StatusPending {
		t.Fatalf("expected only active pending request in list, got %#v", items)
	}
}

func TestPurgeRemovesTerminalRowsAfterRetention(t *testing.T) {
	service := NewService()
	approved, err := service.Create(CreateRequest{DeviceName: "Approved TV"})
	if err != nil {
		t.Fatalf("create pairing: %v", err)
	}
	if _, err := service.Approve(approved.ID, "admin"); err != nil {
		t.Fatalf("approve pairing: %v", err)
	}
	service.mu.Lock()
	item := service.byID[approved.ID]
	item.UpdatedAt = time.Now().UTC().Add(-25 * time.Hour)
	service.byID[approved.ID] = item
	service.mu.Unlock()

	removed, err := service.Purge(24 * time.Hour)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if removed != 1 {
		t.Fatalf("expected one terminal row removed, got %d", removed)
	}
	if _, ok := service.Get(approved.ID); ok {
		t.Fatal("expected old approved request to be purged")
	}
}

func TestPersistentServiceKeepsPendingRequestAcrossServiceRestart(t *testing.T) {
	db, err := database.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close database: %v", err)
		}
	}()

	created, err := NewPersistentService(db).Create(CreateRequest{
		DeviceName:    "Apple TV",
		ClientProfile: "apple-tv",
		DeviceID:      "device-1",
	})
	if err != nil {
		t.Fatalf("create request: %v", err)
	}

	got, ok := NewPersistentService(db).Get(created.ID)
	if !ok {
		t.Fatalf("expected request %s after service restart", created.ID)
	}
	if got.ID != created.ID || got.Status != StatusPending || got.Code == "" {
		t.Fatalf("unexpected restored request: %#v", got)
	}
}

func TestPersistentListOnlyReturnsActivePendingRequests(t *testing.T) {
	db, err := database.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close database: %v", err)
		}
	}()

	service := NewPersistentService(db)
	pending, err := service.Create(CreateRequest{DeviceName: "Pending TV", DeviceID: "pending-device"})
	if err != nil {
		t.Fatalf("create pending pairing: %v", err)
	}
	approved, err := service.Create(CreateRequest{DeviceName: "Approved TV", DeviceID: "approved-device"})
	if err != nil {
		t.Fatalf("create approved pairing: %v", err)
	}
	if _, err := service.Approve(approved.ID, "admin"); err != nil {
		t.Fatalf("approve pairing: %v", err)
	}

	items := service.List()
	if len(items) != 1 || items[0].ID != pending.ID || items[0].Status != StatusPending {
		t.Fatalf("expected only active pending request in persistent list, got %#v", items)
	}
}

func TestExecPairingWriteRetriesSQLiteBusy(t *testing.T) {
	execer := &flakyPairingExecer{remainingBusy: 2}

	if _, err := execPairingWrite(execer, "UPDATE pairing_requests SET status = ?", StatusPending); err != nil {
		t.Fatalf("expected retry to recover from sqlite busy, got %v", err)
	}
	if execer.calls != 3 {
		t.Fatalf("expected three attempts, got %d", execer.calls)
	}
}

type flakyPairingExecer struct {
	remainingBusy int
	calls         int
}

func (f *flakyPairingExecer) Exec(string, ...any) (sql.Result, error) {
	f.calls++
	if f.remainingBusy > 0 {
		f.remainingBusy--
		return nil, errString("database is locked (5) (SQLITE_BUSY)")
	}
	return noRowsResult{}, nil
}

type errString string

func (e errString) Error() string { return string(e) }

type noRowsResult struct{}

func (noRowsResult) LastInsertId() (int64, error) { return 0, nil }
func (noRowsResult) RowsAffected() (int64, error) { return 0, nil }

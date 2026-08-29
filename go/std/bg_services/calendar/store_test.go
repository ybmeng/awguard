package calendar

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func sampleEvent() Event {
	return Event{
		Title:       "standup",
		Description: "daily sync",
		Location:    "room 3",
		Start:       "2024-03-04T09:00:00",
		End:         "2024-03-04T09:30:00",
		TZ:          "America/New_York",
		RRULE:       "FREQ=WEEKLY;BYDAY=MO;COUNT=4",
		EXDATE:      []string{"2024-03-11T09:00:00"},
	}
}

func TestStoreCRUDRoundTrip(t *testing.T) {
	st := newTestStore(t)
	created, err := st.Create(sampleEvent())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !validEventID(created.ID) {
		t.Errorf("Create minted id %q, want evt_+ULID", created.ID)
	}
	if created.CreatedAt.IsZero() || !created.UpdatedAt.Equal(created.CreatedAt) {
		t.Errorf("timestamps = %v / %v, want equal and non-zero", created.CreatedAt, created.UpdatedAt)
	}

	got, err := st.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := created
	want.CreatedAt = got.CreatedAt // instants compared separately: RFC3339Nano round-trip drops monotonic clock
	want.UpdatedAt = got.UpdatedAt
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Get = %+v, want %+v", got, want)
	}
	if !got.CreatedAt.Equal(created.CreatedAt) || !got.UpdatedAt.Equal(created.UpdatedAt) {
		t.Errorf("timestamps drifted through storage: %v vs %v", got.CreatedAt, created.CreatedAt)
	}
}

func TestStoreUpdate(t *testing.T) {
	st := newTestStore(t)
	created, err := st.Create(sampleEvent())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	created.Title = "renamed"
	created.RRULE = ""
	created.EXDATE = nil
	updated, err := st.Update(created)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.UpdatedAt.Before(created.CreatedAt) {
		t.Errorf("UpdatedAt %v is before CreatedAt %v", updated.UpdatedAt, created.CreatedAt)
	}

	got, err := st.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "renamed" || got.RRULE != "" {
		t.Errorf("update not persisted: %+v", got)
	}
	if got.EXDATE == nil || len(got.EXDATE) != 0 {
		t.Errorf("nil EXDATE should persist as empty list, got %#v", got.EXDATE)
	}
	if !got.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("CreatedAt changed on update: %v -> %v", created.CreatedAt, got.CreatedAt)
	}
}

func TestStoreDeleteAndNotFound(t *testing.T) {
	st := newTestStore(t)
	created, err := st.Create(sampleEvent())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := st.Delete(created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := st.Get(created.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after delete = %v, want ErrNotFound", err)
	}
	if _, err := st.Update(created); !errors.Is(err, ErrNotFound) {
		t.Errorf("Update missing = %v, want ErrNotFound", err)
	}
	if err := st.Delete(created.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete missing = %v, want ErrNotFound", err)
	}
}

func TestStoreListInCreationOrder(t *testing.T) {
	st := newTestStore(t)
	first, err := st.Create(Event{Title: "first", Start: "2024-01-01T08:00:00", End: "2024-01-01T09:00:00", TZ: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.Create(Event{Title: "second", Start: "2024-01-02T08:00:00", End: "2024-01-02T09:00:00", TZ: "UTC"})
	if err != nil {
		t.Fatal(err)
	}

	events, err := st.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(events) != 2 || events[0].ID != first.ID || events[1].ID != second.ID {
		t.Errorf("List = %+v, want [first second] in creation order", events)
	}
}

func TestStorePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "calendar.db")
	st, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	created, err := st.Create(sampleEvent())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	st2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	got, err := st2.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got.Title != created.Title || got.Start != created.Start {
		t.Errorf("reopened event = %+v, want %+v", got, created)
	}
}

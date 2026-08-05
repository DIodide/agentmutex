package history

import (
	"testing"
	"time"
)

func TestRecordAndQuery(t *testing.T) {
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	base := time.Now().Add(-time.Hour)
	events := []Event{
		{TS: base, Key: "deploy:staging", Event: EventAcquired, LeaseID: "l1", Agent: "a", TTLSeconds: 60},
		{TS: base.Add(time.Minute), Key: "deploy:staging", Event: EventRenewed, LeaseID: "l1", Agent: "a", TTLSeconds: 60},
		{TS: base.Add(2 * time.Minute), Key: "deploy:staging", Event: EventReleased, LeaseID: "l1", Agent: "a", HeldSeconds: 120},
		{TS: base.Add(3 * time.Minute), Key: "other:key", Event: EventAcquired, LeaseID: "l2", Agent: "b"},
	}
	for _, e := range events {
		if err := l.Record(e); err != nil {
			t.Fatal(err)
		}
	}

	// Default: renews excluded, newest first.
	got, err := l.Query(QueryOpts{Key: "deploy:staging"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Event != EventReleased || got[1].Event != EventAcquired {
		t.Fatalf("default query wrong: %+v", got)
	}
	if got[0].LeaseID != "l1" || got[0].HeldSeconds != 120 {
		t.Fatalf("event fields lost: %+v", got[0])
	}

	// IncludeRenews shows the heartbeat.
	got, err = l.Query(QueryOpts{Key: "deploy:staging", IncludeRenews: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 with renews, got %d", len(got))
	}

	// No key = all keys.
	got, err = l.Query(QueryOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 { // 4 events minus 1 renew
		t.Fatalf("all-keys query: want 3, got %d", len(got))
	}

	// Since filters out old events.
	got, err = l.Query(QueryOpts{Since: base.Add(150 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != "other:key" {
		t.Fatalf("since filter wrong: %+v", got)
	}

	// Limit caps the result.
	got, err = l.Query(QueryOpts{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != "other:key" {
		t.Fatalf("limit wrong: %+v", got)
	}
}

func TestReopenPersists(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Record(Event{Key: "k", Event: EventAcquired, Agent: "a"}); err != nil {
		t.Fatal(err)
	}
	l.Close()

	l2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	got, err := l2.Query(QueryOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Agent != "a" {
		t.Fatalf("events did not persist across reopen: %+v", got)
	}
}

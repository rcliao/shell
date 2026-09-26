package store

import (
	"testing"
	"time"
)

func TestEventsDedupAndLifecycle(t *testing.T) {
	s, done := newTestStore(t)
	defer done()
	at := time.Now().Add(-time.Hour)
	id, created, err := s.AddEvent(Event{Source: "gmail", Kind: "email.received", DedupID: "m1", Summary: "Flight confirmation", Ref: "m1", OccurredAt: at})
	if err != nil || !created || id == 0 {
		t.Fatalf("add: id=%d created=%v err=%v", id, created, err)
	}
	if _, created, _ := s.AddEvent(Event{Source: "gmail", Kind: "email.received", DedupID: "m1", Summary: "again"}); created {
		t.Error("the same source + dedup id must be ignored")
	}
	if _, _, err := s.AddEvent(Event{Source: "gmail", Kind: "x", DedupID: "m2"}); err == nil {
		t.Error("an event without a summary must be refused")
	}
	news, _ := s.ListEvents([]string{EventNew}, 0)
	if len(news) != 1 || news[0].Summary != "Flight confirmation" || news[0].OccurredAt.IsZero() {
		t.Fatalf("new events = %+v", news)
	}
	if err := s.MarkEvent(id, EventDone, "added to the trip doc"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkEvent(id, "archived", ""); err == nil {
		t.Error("unknown status must be refused")
	}
	if err := s.MarkEvent(999, EventDone, ""); err == nil {
		t.Error("unknown id must be an error")
	}
	all, _ := s.ListEvents(nil, 0)
	if len(all) != 1 || all[0].Status != EventDone || all[0].Note != "added to the trip doc" {
		t.Fatalf("after done: %+v", all)
	}
}

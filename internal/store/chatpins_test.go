package store

import "testing"

func TestChatPinAbsentReturnsNil(t *testing.T) {
	s := testStore(t)

	pin, err := s.GetChatPin(42)
	if err != nil {
		t.Fatal(err)
	}
	if pin != nil {
		t.Errorf("pin = %+v, want nil for a chat that never had one", pin)
	}
}

func TestChatPinSetGetAndReplace(t *testing.T) {
	s := testStore(t)

	if err := s.SetChatPin(42, 100); err != nil {
		t.Fatal(err)
	}
	pin, err := s.GetChatPin(42)
	if err != nil {
		t.Fatal(err)
	}
	if pin == nil || pin.ProjectsMsgID != 100 || pin.ChatID != 42 {
		t.Fatalf("pin = %+v, want chat 42 msg 100", pin)
	}
	if pin.UpdatedAt.IsZero() {
		t.Error("updated_at should be stamped")
	}

	// Replacing overwrites in place — one row per chat, PK does the work.
	if err := s.SetChatPin(42, 200); err != nil {
		t.Fatal(err)
	}
	pin, err = s.GetChatPin(42)
	if err != nil {
		t.Fatal(err)
	}
	if pin == nil || pin.ProjectsMsgID != 200 {
		t.Fatalf("pin after replace = %+v, want msg 200", pin)
	}

	// A different chat gets its own row.
	if err := s.SetChatPin(-100200300, 7); err != nil {
		t.Fatal(err)
	}
	other, err := s.GetChatPin(-100200300)
	if err != nil {
		t.Fatal(err)
	}
	if other == nil || other.ProjectsMsgID != 7 {
		t.Fatalf("other pin = %+v, want msg 7", other)
	}
}

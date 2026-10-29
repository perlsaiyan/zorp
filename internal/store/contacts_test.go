package store

import (
	"context"
	"testing"
	"time"
)

func TestContacts(t *testing.T) {
	ctx := context.Background()
	s, _, acct, inbox, _ := setup(t)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	me := Address{Name: "Test", Addr: "test@example.com"}
	ann := Address{Name: "Ann", Addr: "ann@example.com"}
	err := s.ApplyDelta(ctx, acct, inbox.ID, DeltaBatch{Upserts: []Message{
		// An old message from Bob, long ago.
		{GraphID: "1", From: Address{Name: "Bob", Addr: "bob@example.com"}, To: []Address{me}, ReceivedAt: now.AddDate(-1, 0, 0)},
		// Recent mail from Ann, copying Cy.
		{GraphID: "2", From: Address{Addr: "ann@example.com"}, To: []Address{me}, Cc: []Address{{Name: "Cy", Addr: "cy@example.com"}}, ReceivedAt: now.AddDate(0, 0, -1)},
		// The user wrote to Ann (under her name) and to Dee.
		{GraphID: "3", From: me, To: []Address{ann, {Addr: "dee@example.com"}}, ReceivedAt: now.AddDate(0, 0, -2)},
		{GraphID: "4", From: Address{Addr: "noreply"}, ReceivedAt: now},
	}})
	if err != nil {
		t.Fatal(err)
	}
	cs, err := s.Contacts(ctx, acct, "TEST@example.com", now)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, c := range cs {
		order = append(order, c.Addr)
	}
	want := []string{"ann@example.com", "dee@example.com", "cy@example.com", "bob@example.com"}
	if len(order) != len(want) {
		t.Fatalf("contacts = %q", order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("contacts = %q, want %q", order, want)
		}
	}
	if cs[0].Name != "Ann" {
		t.Errorf("Ann's name = %q", cs[0].Name)
	}
}

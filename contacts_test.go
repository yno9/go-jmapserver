package jmapserver

import "testing"

func TestContactsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if got := ReadContacts(dir); got != nil {
		t.Fatalf("expected nil before any write, got %v", got)
	}
	card := Card{
		Type:    "Card",
		Version: "1.0",
		UID:     "urn:uuid:11111111-1111-5111-8111-111111111111",
		Emails:  map[string]EmailAddr{"e1": {Address: "bob@example.com"}},
	}
	if err := PutContact(dir, card); err != nil {
		t.Fatalf("PutContact: %v", err)
	}
	got := ReadContacts(dir)
	if len(got) != 1 || got[0].UID != card.UID || got[0].Emails["e1"].Address != "bob@example.com" {
		t.Fatalf("ReadContacts = %+v, want one card matching %+v", got, card)
	}
}

func TestContactsUpsertReplacesByUID(t *testing.T) {
	dir := t.TempDir()
	uid := "urn:uuid:22222222-2222-5222-8222-222222222222"
	PutContact(dir, Card{Type: "Card", Version: "1.0", UID: uid, Emails: map[string]EmailAddr{"e1": {Address: "old@example.com"}}})
	PutContact(dir, Card{Type: "Card", Version: "1.0", UID: uid, Emails: map[string]EmailAddr{"e1": {Address: "new@example.com"}}})
	got := ReadContacts(dir)
	if len(got) != 1 {
		t.Fatalf("expected upsert to replace, not append — got %d cards", len(got))
	}
	if got[0].Emails["e1"].Address != "new@example.com" {
		t.Fatalf("expected replaced address new@example.com, got %s", got[0].Emails["e1"].Address)
	}
}

func TestContactsMultipleCards(t *testing.T) {
	dir := t.TempDir()
	PutContact(dir, Card{Type: "Card", Version: "1.0", UID: "urn:uuid:1"})
	PutContact(dir, Card{Type: "Card", Version: "1.0", UID: "urn:uuid:2"})
	got := ReadContacts(dir)
	if len(got) != 2 {
		t.Fatalf("expected 2 distinct cards, got %d", len(got))
	}
}

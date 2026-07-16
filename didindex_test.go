package jmapserver

import "testing"

func TestLocalDIDIndex(t *testing.T) {
	dir := t.TempDir()
	did := "did:dht:testabc"
	if got := LookupLocalDID(dir, did); got != nil {
		t.Fatalf("expected nil before recording, got %v", got)
	}
	RecordLocalDID(dir, did, "y@biset.md")
	if got := LookupLocalDID(dir, did); len(got) != 1 || got[0] != "y@biset.md" {
		t.Fatalf("LookupLocalDID = %v, want [y@biset.md]", got)
	}
}

func TestLocalDIDIndexMultipleAddresses(t *testing.T) {
	dir := t.TempDir()
	did := "did:dht:testabc"
	RecordLocalDID(dir, did, "y@biset.md")
	RecordLocalDID(dir, did, "f@orillo.org")
	got := LookupLocalDID(dir, did)
	if len(got) != 2 || got[0] != "y@biset.md" || got[1] != "f@orillo.org" {
		t.Fatalf("LookupLocalDID = %v, want [y@biset.md f@orillo.org]", got)
	}
}

func TestLocalDIDIndexIdempotent(t *testing.T) {
	dir := t.TempDir()
	did := "did:dht:testabc"
	RecordLocalDID(dir, did, "y@biset.md")
	RecordLocalDID(dir, did, "y@biset.md") // duplicate, must not double-record
	got := LookupLocalDID(dir, did)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 entry after duplicate record, got %v", got)
	}
}

func TestRemoveLocalDIDDropsOneAddressKeepsOthers(t *testing.T) {
	dir := t.TempDir()
	did := "did:dht:testabc"
	RecordLocalDID(dir, did, "y@biset.md")
	RecordLocalDID(dir, did, "f@orillo.org")
	if err := RemoveLocalDID(dir, did, "y@biset.md"); err != nil {
		t.Fatalf("RemoveLocalDID: %v", err)
	}
	got := LookupLocalDID(dir, did)
	if len(got) != 1 || got[0] != "f@orillo.org" {
		t.Fatalf("LookupLocalDID after remove = %v, want [f@orillo.org]", got)
	}
}

func TestRemoveLocalDIDLastAddressRemovesFile(t *testing.T) {
	dir := t.TempDir()
	did := "did:dht:testabc"
	RecordLocalDID(dir, did, "y@biset.md")
	if err := RemoveLocalDID(dir, did, "y@biset.md"); err != nil {
		t.Fatalf("RemoveLocalDID: %v", err)
	}
	if got := LookupLocalDID(dir, did); got != nil {
		t.Fatalf("expected nil after removing last address, got %v", got)
	}
}

func TestRemoveLocalDIDNoOpWhenNoIndex(t *testing.T) {
	dir := t.TempDir()
	if err := RemoveLocalDID(dir, "did:dht:nonexistent", "y@biset.md"); err != nil {
		t.Fatalf("RemoveLocalDID on missing index should be a no-op, got err: %v", err)
	}
}

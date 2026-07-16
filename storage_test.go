package jmapserver

import (
	"os"
	"path/filepath"
	"testing"
)

func setupTestAccount(t *testing.T, dataDir, domain, localpart string) string {
	t.Helper()
	acctDir := filepath.Join(dataDir, domain, localpart)
	if err := os.MkdirAll(filepath.Join(acctDir, "messages"), 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"mailboxes.json":     `[{"id":"inbox"}]`,
		"delta.json":         `1`,
		"auth_token_hash":    "somehash",
		"messages/msg1.json": `{"id":"msg1"}`,
		"messages/msg2.json": `{"id":"msg2"}`,
	}
	for rel, content := range files {
		p := filepath.Join(acctDir, rel)
		if err := os.WriteFile(p, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return acctDir
}

func TestListAccountStorageSummarizesMessages(t *testing.T) {
	dir := t.TempDir()
	setupTestAccount(t, dir, "example.com", "alice")

	entries, err := listAccountStorage(dir, "example.com", "alice")
	if err != nil {
		t.Fatalf("listAccountStorage: %v", err)
	}

	var msgEntry *StorageEntry
	names := map[string]bool{}
	for i := range entries {
		names[entries[i].Name] = true
		if entries[i].Name == "messages" {
			msgEntry = &entries[i]
		}
	}
	for _, want := range []string{"mailboxes.json", "delta.json", "auth_token_hash", "messages"} {
		if !names[want] {
			t.Errorf("missing expected entry %q, got %+v", want, entries)
		}
	}
	if msgEntry == nil {
		t.Fatal("messages entry not found")
	}
	if msgEntry.Type != "dir" || msgEntry.Count != 2 {
		t.Errorf("messages entry = %+v, want type=dir count=2", msgEntry)
	}
}

func TestExportAccountStorageReadsEveryFile(t *testing.T) {
	dir := t.TempDir()
	setupTestAccount(t, dir, "example.com", "bob")

	files, err := exportAccountStorage(dir, "example.com", "bob")
	if err != nil {
		t.Fatalf("exportAccountStorage: %v", err)
	}
	want := map[string]string{
		"mailboxes.json":     `[{"id":"inbox"}]`,
		"delta.json":         `1`,
		"auth_token_hash":    "somehash",
		"messages/msg1.json": `{"id":"msg1"}`,
		"messages/msg2.json": `{"id":"msg2"}`,
	}
	for path, wantContent := range want {
		got, ok := files[path]
		if !ok {
			t.Errorf("missing file %q in export", path)
			continue
		}
		if string(got) != wantContent {
			t.Errorf("file %q = %q, want %q", path, got, wantContent)
		}
	}
	if len(files) != len(want) {
		t.Errorf("exported %d files, want %d (got: %v)", len(files), len(want), files)
	}
}

func TestListAccountStorageMissingAccount(t *testing.T) {
	dir := t.TempDir()
	if _, err := listAccountStorage(dir, "example.com", "nobody"); err == nil {
		t.Error("expected error for a nonexistent account directory")
	}
}

func TestListMessageFilesDrillsDownIntoMessages(t *testing.T) {
	dir := t.TempDir()
	setupTestAccount(t, dir, "example.com", "carol")

	files, err := listMessageFiles(dir, "example.com", "carol")
	if err != nil {
		t.Fatalf("listMessageFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2: %+v", len(files), files)
	}
	byName := map[string]StorageEntry{}
	for _, f := range files {
		byName[f.Name] = f
	}
	if f, ok := byName["msg1.json"]; !ok || f.Type != "file" || f.SizeBytes == 0 {
		t.Errorf("msg1.json entry = %+v, ok=%v", f, ok)
	}
	if f, ok := byName["msg2.json"]; !ok || f.Type != "file" || f.SizeBytes == 0 {
		t.Errorf("msg2.json entry = %+v, ok=%v", f, ok)
	}
}

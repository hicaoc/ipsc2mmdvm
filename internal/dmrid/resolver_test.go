package dmrid

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAndLookup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dmrid.csv")
	content := "1023092,VE3FIS,Tom,,Toronto,Ontario,Canada\n1023093,VE3GZS,Zygmunt Piotr,,Ottawa,Ontario,Canada\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write test file: %v", err)
	}

	resolver, err := Load(path)
	if err != nil {
		t.Fatalf("load resolver: %v", err)
	}

	if got := resolver.Lookup(1023092); got != "VE3FIS" {
		t.Fatalf("expected VE3FIS, got %q", got)
	}
	if got := resolver.Lookup(9999999); got != "" {
		t.Fatalf("expected empty lookup, got %q", got)
	}
}

func TestLoadSemicolonFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dmrid.txt")
	content := "1023007;VA3BOC;\n1023016;VE3IAO;\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write test file: %v", err)
	}

	resolver, err := Load(path)
	if err != nil {
		t.Fatalf("load resolver: %v", err)
	}

	if got := resolver.Lookup(1023007); got != "VA3BOC" {
		t.Fatalf("expected VA3BOC, got %q", got)
	}
	if got := resolver.Lookup(1023016); got != "VE3IAO" {
		t.Fatalf("expected VE3IAO, got %q", got)
	}
}

func TestLookupIDAndNormalizeCallsign(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dmrid.csv")
	content := "1023092,VE3FIS,Tom,,Toronto,Ontario,Canada\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write test file: %v", err)
	}

	resolver, err := Load(path)
	if err != nil {
		t.Fatalf("load resolver: %v", err)
	}

	if got := resolver.LookupID("ve3fis"); got != 1023092 {
		t.Fatalf("expected 1023092, got %d", got)
	}
	if got := NormalizeCallsign(" ve3fis "); got != "VE3FIS" {
		t.Fatalf("expected VE3FIS, got %q", got)
	}
}

func TestIsValidCallsign(t *testing.T) {
	if !IsValidCallsign("VE3FIS") {
		t.Fatal("expected VE3FIS to be valid")
	}
	if IsValidCallsign("INVALID") {
		t.Fatal("expected INVALID to be rejected")
	}
}

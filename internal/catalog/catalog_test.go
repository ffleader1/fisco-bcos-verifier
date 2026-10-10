package catalog_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"buoyup/fisco/internal/catalog"
)

func TestBuiltinCatalog(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{"VN", "CN"} {
		reg, ok := c.Region(r)
		if !ok || len(reg.Provinces) < 10 {
			t.Fatalf("region %s: %+v", r, reg)
		}
		banks := c.Banks(r)
		if len(banks) != 5 {
			t.Fatalf("region %s has %d banks", r, len(banks))
		}
		for _, b := range banks {
			if b.Currency != reg.Currency || !b.Mock || b.Policy.Hash.IsZero() || b.Contract.Hash.IsZero() {
				t.Fatalf("bank %s not completed: %+v", b.Code, b)
			}
			if b.Policy.Summary == "" || len(b.Policy.Sectors.Prefer) == 0 || len(b.Policy.Sectors.Avoid) == 0 || len(b.Policy.Regions.Prefer) == 0 || len(b.Policy.RedFlags) == 0 {
				t.Fatalf("bank %s policy too thin", b.Code)
			}
			if st := b.ContractStatus(time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)); st != "active" {
				t.Fatalf("bank %s contract %s", b.Code, st)
			}
		}
	}
	if _, ok := c.Bank("vcb"); !ok {
		t.Fatal("lookup must be case-insensitive")
	}
}

func TestCatalogFromDirRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	src, _ := os.ReadFile("data/regions.yaml")
	must(os.WriteFile(filepath.Join(dir, "regions.yaml"), src, 0o644))
	must(os.MkdirAll(filepath.Join(dir, "banks"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "banks", "X.yaml"), []byte("code: X\nname: X\nregion: VN\ncolour: red\n"), 0o644))
	if _, err := catalog.Load(dir); err == nil {
		t.Fatal("expected an error for an unknown field")
	}
	must(os.WriteFile(filepath.Join(dir, "banks", "X.yaml"), []byte("code: X\nname: X\nregion: VN\nheadquarters: CN-GD\n"), 0o644))
	if _, err := catalog.Load(dir); err == nil {
		t.Fatal("expected an error for a province outside the region")
	}
	must(os.WriteFile(filepath.Join(dir, "banks", "X.yaml"), []byte("code: X\nname: X\nregion: VN\nactive: true\ncontract:\n  status: active\n  expiresOn: \"2020-01-01\"\n"), 0o644))
	c, err := catalog.Load(dir)
	must(err)
	b, _ := c.Bank("X")
	if st := b.ContractStatus(time.Now()); st != "expired" {
		t.Fatalf("status %s", st)
	}
}

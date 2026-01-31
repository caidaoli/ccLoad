package storage

import "testing"

func TestParseModelsForMigration(t *testing.T) {
	got, err := parseModelsForMigration("")
	if err != nil || got != nil {
		t.Fatalf("empty: got (%v,%v), want (nil,nil)", got, err)
	}
	got, err = parseModelsForMigration("[]")
	if err != nil || got != nil {
		t.Fatalf("[]: got (%v,%v), want (nil,nil)", got, err)
	}

	got, err = parseModelsForMigration(`["a","b"]`)
	if err != nil || len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("valid: got (%v,%v), want ([a b],nil)", got, err)
	}

	if _, err := parseModelsForMigration(`{}`); err == nil {
		t.Fatalf("expected error on invalid json")
	}
}

func TestParseModelRedirectsForMigration(t *testing.T) {
	got, err := parseModelRedirectsForMigration("")
	if err != nil || got != nil {
		t.Fatalf("empty: got (%v,%v), want (nil,nil)", got, err)
	}
	got, err = parseModelRedirectsForMigration("{}")
	if err != nil || got != nil {
		t.Fatalf("{}: got (%v,%v), want (nil,nil)", got, err)
	}

	got, err = parseModelRedirectsForMigration(`{"a":"b"}`)
	if err != nil || got["a"] != "b" {
		t.Fatalf("valid: got (%v,%v), want map[a:b]", got, err)
	}

	if _, err := parseModelRedirectsForMigration(`[]`); err == nil {
		t.Fatalf("expected error on invalid json")
	}
}

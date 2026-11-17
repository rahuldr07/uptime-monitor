package store

import (
	"context"
	"testing"
	"time"
	_ "time/tzdata" // the tests must not depend on the host's tz database
)

func TestZoneNamesLoad(t *testing.T) {
	for _, n := range ZoneNames() {
		if LoadZone(n) == nil {
			t.Errorf("zone %q does not load", n)
		}
	}
}

func TestLoadZoneValidation(t *testing.T) {
	for _, ok := range []string{"UTC", "Asia/Kolkata", "America/New_York", "Asia/Calcutta"} {
		if LoadZone(ok) == nil {
			t.Errorf("LoadZone(%q) = nil, want a zone", ok)
		}
	}
	// "" and "Local" load in time.LoadLocation but are not zones to store.
	for _, bad := range []string{"", "Local", "Not/AZone", "../etc/passwd", "/etc/localtime", "Asia/Kolkata\x00", "asia/kolkata ", "localtime", "posixrules", "Factory", "Asia/./Kolkata", "Asia//Kolkata", "./UTC", string(make([]byte, 65))} {
		if LoadZone(bad) != nil {
			t.Errorf("LoadZone(%q) loaded, want nil", bad)
		}
	}
}

func TestZoneAliases(t *testing.T) {
	for alias, target := range zoneAliases {
		if LoadZone(alias) == nil || !containsStr(zoneNames, target) {
			t.Errorf("alias %q -> %q: loads=%v listed=%v", alias, target, LoadZone(alias) != nil, containsStr(zoneNames, target))
		}
	}
	for alias, want := range map[string]string{"Asia/Calcutta": "Asia/Kolkata", "Asia/Saigon": "Asia/Ho_Chi_Minh", "Asia/Katmandu": "Asia/Kathmandu",
		"Asia/Rangoon": "Asia/Yangon", "Europe/Kiev": "Europe/Kyiv", "America/Buenos_Aires": "America/Argentina/Buenos_Aires", "UTC": "UTC"} {
		if got := CanonicalZone(alias); got != want || LoadZone(alias) == nil {
			t.Errorf("CanonicalZone(%q) = %q, want %q", alias, got, want)
		}
	}
}

func containsStr(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestEffectiveZonePrecedence(t *testing.T) {
	cases := []struct{ explicit, detected, want string }{
		{"", "", "UTC"},
		{"", "Asia/Kolkata", "Asia/Kolkata"},
		{"America/New_York", "Asia/Kolkata", "America/New_York"},
		{"America/New_York", "", "America/New_York"},
		{"Gone/Zone", "Asia/Kolkata", "Asia/Kolkata"}, // a stored name that no longer loads is skipped
		{"Gone/Zone", "Also/Gone", "UTC"},
	}
	for _, c := range cases {
		if got := EffectiveZone(c.explicit, c.detected).String(); got != c.want {
			t.Errorf("EffectiveZone(%q, %q) = %s, want %s", c.explicit, c.detected, got, c.want)
		}
	}
}

func TestSetZones(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	u, ws, err := s.CreateAccount(ctx, AccountInput{Email: "a@x.co", PasswordHash: "h", WorkspaceName: "A", Slug: "a", Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetTimeZone(ctx, u.ID, "Nope/Nope"); err != ErrInvalidZone {
		t.Fatalf("SetTimeZone junk = %v", err)
	}
	if err := s.SetDetectedZone(ctx, u.ID, ""); err != ErrInvalidZone {
		t.Fatalf("SetDetectedZone empty = %v", err)
	}
	if err := s.SetDetectedZone(ctx, u.ID, "Asia/Calcutta"); err != nil {
		t.Fatal(err)
	}
	if loc := ws.OwnerLocation(ctx); loc.String() != "Asia/Calcutta" {
		t.Fatalf("owner zone = %s, want the detected alias as reported", loc)
	}
	if err := s.SetTimeZone(ctx, u.ID, "America/New_York"); err != nil {
		t.Fatal(err)
	}
	if loc := ws.OwnerLocation(ctx); loc.String() != "America/New_York" {
		t.Fatalf("owner zone = %s, want the explicit choice", loc)
	}
	if err := s.SetTimeZone(ctx, u.ID, ""); err != nil {
		t.Fatal(err)
	}
	if loc := ws.OwnerLocation(ctx); loc.String() != "Asia/Calcutta" {
		t.Fatalf("owner zone = %s, want automatic (detected) again", loc)
	}
}

func TestOwnedWorkspaceIDs(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	u, ws, err := s.CreateAccount(ctx, AccountInput{Email: "a@x.co", PasswordHash: "h", WorkspaceName: "A", Slug: "a", Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	ids, err := s.OwnedWorkspaceIDs(ctx, u.ID)
	if err != nil || len(ids) != 1 || ids[0] != ws.ID() {
		t.Fatalf("ids = %v, %v", ids, err)
	}
}

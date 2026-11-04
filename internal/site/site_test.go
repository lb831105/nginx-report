package site

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalize(t *testing.T) {
	tests := []struct {
		raw      string
		stripWWW bool
		wantKey  string
		wantKind Kind
		ok       bool
	}{
		{"example.com", false, "d:example.com", KindDomain, true},
		{"HTTPS://WWW.Example.COM:443/a?b=1", true, "d:example.com", KindDomain, true},
		{"//api.example.com/v1", false, "d:api.example.com", KindDomain, true},
		{"example.com:8080", false, "d:example.com", KindDomain, true},
		{"example.com.", false, "d:example.com", KindDomain, true},
		{"*.example.com", false, "d:example.com", KindDomain, true},
		{"example.com www.example.com", false, "d:example.com", KindDomain, true},
		{"203.0.113.10", false, "i:203.0.113.10", KindIP, true},
		{"[2001:db8::1]:443", false, "i:2001:db8::1", KindIP, true},
		{"http://203.0.113.10:8080/x", false, "i:203.0.113.10", KindIP, true},
		{"-", false, "", KindUnknown, false},
		{"unknown", false, "", KindUnknown, false},
		{"bad host", false, "", KindUnknown, false},
		{"a..b", false, "", KindUnknown, false},
	}
	for _, tt := range tests {
		got, ok := Normalize(tt.raw, tt.stripWWW)
		if ok != tt.ok {
			t.Fatalf("Normalize(%q) ok=%v want %v (%+v)", tt.raw, ok, tt.ok, got)
		}
		if !ok {
			continue
		}
		if got.Key != tt.wantKey || got.Kind != tt.wantKind {
			t.Fatalf("Normalize(%q)=%+v want key=%s kind=%s", tt.raw, got, tt.wantKey, tt.wantKind)
		}
	}
}

func TestApplyAliasChain(t *testing.T) {
	aliases := map[string]string{
		"a.example.com": "b.example.com",
		"b.example.com": "example.com",
	}
	got := ApplyAlias(Info{Key: "d:a.example.com", Name: "a.example.com", Kind: KindDomain}, aliases)
	if got.Name != "example.com" || got.Key != "d:example.com" {
		t.Fatalf("alias chain = %+v", got)
	}
	// 环不能死循环
	loop := map[string]string{"a": "b", "b": "a"}
	got = ApplyAlias(Info{Key: "d:a", Name: "a", Kind: KindDomain}, loop)
	if got.Name != "a" && got.Name != "b" {
		t.Fatalf("alias loop = %+v", got)
	}
}

func TestFromFilename(t *testing.T) {
	tests := []struct {
		name    string
		wantKey string
		ok      bool
	}{
		{"example.com.log", "d:example.com", true},
		{"www.example.com_2026-10-30.log", "d:www.example.com", true},
		{"example.com_2026-10-30.log.gz", "d:example.com", true},
		{"api.example.com-access.log", "d:api.example.com", true},
		{"api.example.com_access.log", "d:api.example.com", true},
		{"api.example.com.access.log", "d:api.example.com", true},
		{"access-example.com.log", "d:example.com", true},
		{"1.2.3.4.log", "i:1.2.3.4", true},
		{"access.log", "", false},
		{"error.log", "", false},
		{"28888_access_json.log", "", false},
		{"2026-10-30.log", "", false},
		{"nginx.log", "", false},
	}
	for _, tt := range tests {
		got, ok := FromFilename(tt.name, false)
		if ok != tt.ok {
			t.Fatalf("FromFilename(%q) ok=%v want %v (%+v)", tt.name, ok, tt.ok, got)
		}
		if ok && got.Key != tt.wantKey {
			t.Fatalf("FromFilename(%q)=%+v want %s", tt.name, got, tt.wantKey)
		}
	}
}

func TestSafeDir(t *testing.T) {
	if got := SafeDir("d:Example.COM"); got != filepath.Join("domain", "example.com") {
		t.Fatalf("domain dir = %q", got)
	}
	if got := SafeDir("i:203.0.113.10"); got != filepath.Join("ip", "203.0.113.10") {
		t.Fatalf("ip dir = %q", got)
	}
	if got := SafeDir(UnknownKey); got != "unknown" {
		t.Fatalf("unknown dir = %q", got)
	}
}

func TestLoadMapAndResolver(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mapPath := filepath.Join(dir, "sites.conf")
	content := "example.com = example.com_*.log\napi.example.com = /var/log/nginx/api.log # comment\n"
	if err := os.WriteFile(mapPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	aliasPath := filepath.Join(dir, "alias.conf")
	apiHost := "api." + "example.com"
	if err := os.WriteFile(aliasPath, []byte("www.example.com = example.com\n"+apiHost+" = example.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewResolver(logDir, false)
	if err := r.LoadMap(mapPath); err != nil {
		t.Fatal(err)
	}
	if err := r.LoadAliases(aliasPath); err != nil {
		t.Fatal(err)
	}
	got := r.ResolveFile(filepath.Join(logDir, "example.com_2026-10-30.log"))
	if got.Key != "d:example.com" || got.Source != "map" {
		t.Fatalf("map resolve = %+v", got)
	}
	got = r.ResolveFile(filepath.Join(logDir, "api.example.com.log"))
	if got.Key != "d:example.com" {
		t.Fatalf("filename alias resolve = %+v", got)
	}
	if err := r.SetForced("api.example.com"); err != nil {
		t.Fatal(err)
	}
	got = r.ResolveFile(filepath.Join(logDir, "whatever.log"))
	if got.Key != "d:api.example.com" || got.Source != "name" {
		t.Fatalf("forced resolve = %+v", got)
	}
}

func TestParseFilter(t *testing.T) {
	aliases := map[string]string{"www.example.com": "example.com"}
	got, err := ParseFilter("example.com, 203.0.113.10, -", false, aliases)
	if err == nil {
		t.Fatal("invalid filter should fail")
	}
	got, err = ParseFilter("www.example.com,203.0.113.10", false, aliases)
	if err != nil {
		t.Fatal(err)
	}
	if !got["d:example.com"] || !got["i:203.0.113.10"] {
		t.Fatalf("filter = %+v", got)
	}
}

func TestLoadMapRejectsBadLine(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.conf")
	if err := os.WriteFile(p, []byte("not a mapping\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := NewResolver(dir, false).LoadMap(p); err == nil {
		t.Fatal("bad map should fail")
	}
}

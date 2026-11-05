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

func TestNormalizeWithCount(t *testing.T) {
	info, ok, n := NormalizeWithCount("a.example.com b.example.com", false)
	if !ok || n != 2 || info.Name != "a.example.com" {
		t.Fatalf("got %+v ok=%v n=%d", info, ok, n)
	}
	_, ok, n = NormalizeWithCount("bad host", false)
	if ok || n != 0 {
		t.Fatalf("bad host ok=%v n=%d", ok, n)
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
		{"example.com.2026-10-30.log", "d:example.com", true},
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
	r.Mode = "filename"
	if got := r.ResolveFile(filepath.Join(logDir, "example.com_2026-10-30.log")); got.Key != "d:example.com" {
		t.Fatalf("filename-only mapped file should use filename, got %+v", got)
	}
	r.Mode = "map"
	if got := r.ResolveFile(filepath.Join(logDir, "api.example.com.log")); got.Key != UnknownKey {
		t.Fatalf("map-only unmatched file should be unknown, got %+v", got)
	}
	r.Mode = "auto"
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

func TestFromDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	path := filepath.Join(root, "ad.kmrd.cn", "access.log")
	if got, ok := FromDir(path, root, false); !ok || got.Key != "d:ad.kmrd.cn" {
		t.Fatalf("FromDir=%+v ok=%v", got, ok)
	}
	nested := filepath.Join(root, "ad.kmrd.cn", "2026", "11", "04", "access.log")
	if got, ok := FromDir(nested, root, false); !ok || got.Key != "d:ad.kmrd.cn" {
		t.Fatalf("nested FromDir=%+v ok=%v", got, ok)
	}
	ipPath := filepath.Join(root, "203.0.113.10", "access.log")
	if got, ok := FromDir(ipPath, root, false); !ok || got.Key != "i:203.0.113.10" {
		t.Fatalf("ip FromDir=%+v ok=%v", got, ok)
	}
	if _, ok := FromDir(filepath.Join(root, "access.log"), root, false); ok {
		t.Fatal("generic root file should not infer site")
	}
	// 单文件模式：-logdir /data/_log/ad.kmrd.cn/access.log 时 root 就是站点目录
	siteRoot := filepath.Join(root, "ad.kmrd.cn")
	if got, ok := FromDir(filepath.Join(siteRoot, "access.log"), siteRoot, false); !ok || got.Key != "d:ad.kmrd.cn" {
		t.Fatalf("site root FromDir=%+v ok=%v", got, ok)
	}
}

func TestResolverModeDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	r := NewResolver(root, false)
	r.Mode = "auto"
	got := r.ResolveFile(filepath.Join(root, "shop.kmrd.cn", "access.log"))
	if got.Key != "d:shop.kmrd.cn" || got.Source != "dir" {
		t.Fatalf("auto dir resolve = %+v", got)
	}
	r.Mode = "filename"
	got = r.ResolveFile(filepath.Join(root, "shop.kmrd.cn", "access.log"))
	if got.Key != UnknownKey {
		t.Fatalf("filename-only = %+v", got)
	}
}

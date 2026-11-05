package analyzer

import (
	"strings"
	"testing"

	"nginx-report/internal/site"
)

func TestProcessReaderSitesFromJSONFields(t *testing.T) {
	lines := strings.Join([]string{
		`{"@timestamp":"2026-10-30T10:00:00+08:00","domain":"www.Example.com","client":"1.1.1.1","status":"200","request_uri":"/a","upstream_response_time":"0.010"}`,
		`{"@timestamp":"2026-10-30T10:01:00+08:00","host":"203.0.113.10","client":"2.2.2.2","status":"500","request_uri":"/b","upstream_response_time":"0.020"}`,
		`{"@timestamp":"2026-10-30T10:02:00+08:00","client":"3.3.3.3","status":"404","request_uri":"/c","upstream_response_time":"0.030"}`,
	}, "\n")
	st, err := ProcessReader(strings.NewReader(lines), Options{
		SiteEnabled:  true,
		SiteStripWWW: true,
		SiteIPMode:   "separate",
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.Agg.Total != 3 || len(st.Sites) != 3 {
		t.Fatalf("total=%d sites=%d (%+v)", st.Agg.Total, len(st.Sites), st.Sites)
	}
	want := map[string]int64{"d:example.com": 1, "i:203.0.113.10": 1, "u:unknown": 1}
	var sum int64
	for k, n := range want {
		ss := st.Sites[k]
		if ss == nil || ss.Agg.Total != n {
			t.Fatalf("site %s = %+v, want total %d", k, ss, n)
		}
		sum += ss.Agg.Total
	}
	if sum != st.Agg.Total {
		t.Fatalf("site sum %d != total %d", sum, st.Agg.Total)
	}
	if st.Sites["d:example.com"].Name != "example.com" || st.Sites["d:example.com"].Kind != site.KindDomain {
		t.Fatalf("normalized site = %+v", st.Sites["d:example.com"])
	}
}

func TestProcessReaderSiteFieldAndForced(t *testing.T) {
	lines := strings.Join([]string{
		`{"@timestamp":"2026-10-30T10:00:00+08:00","domain":"a.example.com","host":"b.example.com","client":"1.1.1.1","status":"200","request_uri":"/a","upstream_response_time":"0.010"}`,
		`{"@timestamp":"2026-10-30T10:01:00+08:00","client":"2.2.2.2","status":"200","request_uri":"/b","upstream_response_time":"0.020"}`,
	}, "\n")
	st, err := ProcessReader(strings.NewReader(lines), Options{
		SiteEnabled: true,
		SiteField:   "domain",
		SiteDefault: "d:fallback.example.com",
		SiteForce:   true,
		SiteIPMode:  "inherit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Sites) != 1 || st.Sites["d:fallback.example.com"] == nil || st.Sites["d:fallback.example.com"].Agg.Total != 2 {
		t.Fatalf("forced sites = %+v", st.Sites)
	}
}

func TestProcessReaderSiteMaxOverflowPreservesTotal(t *testing.T) {
	lines := strings.Join([]string{
		`{"@timestamp":"2026-10-30T10:00:00+08:00","domain":"a.example.com","client":"1.1.1.1","status":"200","request_uri":"/a","upstream_response_time":"0.010"}`,
		`{"@timestamp":"2026-10-30T10:01:00+08:00","domain":"b.example.com","client":"2.2.2.2","status":"200","request_uri":"/b","upstream_response_time":"0.020"}`,
		`{"@timestamp":"2026-10-30T10:02:00+08:00","domain":"c.example.com","client":"3.3.3.3","status":"200","request_uri":"/c","upstream_response_time":"0.030"}`,
	}, "\n")
	st, err := ProcessReader(strings.NewReader(lines), Options{SiteEnabled: true, SiteMax: 2, SiteIPMode: "separate"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Agg.Total != 3 {
		t.Fatalf("total=%d", st.Agg.Total)
	}
	if st.Sites["o:other"] == nil || st.Sites["o:other"].Agg.Total != 1 {
		t.Fatalf("overflow = %+v", st.Sites)
	}
	var sum int64
	for _, ss := range st.Sites {
		sum += ss.Agg.Total
	}
	if sum != 3 {
		t.Fatalf("site sum=%d", sum)
	}
}

func TestFileStatsMergeSites(t *testing.T) {
	mk := func(domain, day string) *FileStats {
		line := `{"@timestamp":"` + day + `T10:00:00+08:00","domain":"` + domain + `","client":"1.1.1.1","status":"200","request_uri":"/a","upstream_response_time":"0.010"}`
		st, err := ProcessReader(strings.NewReader(line), Options{SiteEnabled: true, SiteIPMode: "separate"})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	a := mk("a.example.com", "2026-10-30")
	b := mk("a.example.com", "2026-10-31")
	a.Merge(b)
	if a.Agg.Total != 2 || a.Sites["d:a.example.com"] == nil || a.Sites["d:a.example.com"].Agg.Total != 2 {
		t.Fatalf("merged = total %d sites %+v", a.Agg.Total, a.Sites)
	}
}

func TestSiteIPModeAndInvalidFallback(t *testing.T) {
	line := `{"@timestamp":"2026-10-30T10:00:00+08:00","host":"203.0.113.10","client":"1.1.1.1","status":"200","request_uri":"/a","upstream_response_time":"0.010"}`
	base := Options{SiteEnabled: true, SiteDefault: "d:example.com", SiteDefaultSource: "map"}
	for _, mode := range []string{"inherit", "ignore"} {
		opt := base
		opt.SiteIPMode = mode
		st, err := ProcessReader(strings.NewReader(line), opt)
		if err != nil {
			t.Fatal(err)
		}
		if st.Agg.Total != 1 || st.Sites["d:example.com"] == nil || st.Sites["d:example.com"].Agg.Total != 1 {
			t.Fatalf("mode=%s sites=%+v", mode, st.Sites)
		}
	}
	opt := base
	opt.SiteIPMode = "separate"
	st, err := ProcessReader(strings.NewReader(line), opt)
	if err != nil {
		t.Fatal(err)
	}
	if st.Sites["i:203.0.113.10"] == nil || st.Sites["d:example.com"] != nil {
		t.Fatalf("separate sites=%+v", st.Sites)
	}

	invalid := `{"@timestamp":"2026-10-30T10:00:00+08:00","host":"bad host","client":"1.1.1.1","status":"200","request_uri":"/a","upstream_response_time":"0.010"}`
	st, err = ProcessReader(strings.NewReader(invalid), base)
	if err != nil {
		t.Fatal(err)
	}
	ss := st.Sites["d:example.com"]
	if ss == nil || ss.Agg.Total != 1 || ss.Invalid != 1 {
		t.Fatalf("invalid fallback sites=%+v", st.Sites)
	}
}

func TestSiteConflictCountedButKeepsLine(t *testing.T) {
	line := `{"@timestamp":"2026-10-30T10:00:00+08:00","domain":"shop.example.com","client":"1.1.1.1","status":"200","request_uri":"/a","upstream_response_time":"0.010"}`
	st, err := ProcessReader(strings.NewReader(line), Options{
		SiteEnabled: true, SiteDefault: "d:example.com", SiteDefaultSource: "map", SiteIPMode: "separate",
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.Agg.Total != 1 || st.Sites["d:shop.example.com"] == nil || st.Sites["d:shop.example.com"].Agg.Total != 1 {
		t.Fatalf("sites=%+v", st.Sites)
	}
	if st.Sites["d:shop.example.com"].Conflicts != 1 {
		t.Fatalf("conflicts=%d", st.Sites["d:shop.example.com"].Conflicts)
	}
}

func TestSiteMultiDomainCountedFirstUsed(t *testing.T) {
	line := `{"@timestamp":"2026-10-30T10:00:00+08:00","domain":"a.example.com b.example.com","client":"1.1.1.1","status":"200","request_uri":"/a","upstream_response_time":"0.010"}`
	st, err := ProcessReader(strings.NewReader(line), Options{SiteEnabled: true, SiteIPMode: "separate"})
	if err != nil {
		t.Fatal(err)
	}
	ss := st.Sites["d:a.example.com"]
	if ss == nil || ss.Agg.Total != 1 || ss.MultiDomainLines != 1 {
		t.Fatalf("sites=%+v", st.Sites)
	}
}

package analyzer

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 生成与生产日志同构的样本（约 700 字节/行）
func genSample(n int) string {
	var b []byte
	for i := 0; i < n; i++ {
		line := fmt.Sprintf(`{"@timestamp": "2026-09-01T10:%02d:%02d+08:00", "@version": "1", "log_type": "nginx_access", "host": "[IP]", "client": "10.20.30.%d", "real_client": "", "size": "%d", "responsetime": "0.00%d", "domain": "[IP]", "url": "GET /bbt-api/app/enum?name=FillingFrequencyEnum HTTP/1.1", "refer": "", "agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36", "status": "%d", "trace_id": "b1ffb2371c292816053b8dcf8452eaca", "upstream_addr": "[IP]:9510", "upstream_status": "%d", "upstream_response_time": "0.00%d", "request_method": "GET", "request_uri": "/bbt-api/app/enum?name=FillingFrequencyEnum"}`,
			i/60%60, i%60, i%250, 100+i%500, i%9, 200+i%3*100, 200, i%9)
		b = append(b, line...)
		b = append(b, '\n')
	}
	return string(b)
}

func writeSample(tb testing.TB, n int) string {
	tb.Helper()
	dir := tb.TempDir()
	f := filepath.Join(dir, "access.log")
	if err := os.WriteFile(f, []byte(genSample(n)), 0644); err != nil {
		tb.Fatal(err)
	}
	return f
}

// BenchmarkParseFast 字段直查 + 手写时间解析
func BenchmarkParseFast(b *testing.B) {
	f := writeSample(b, 20000)
	info, _ := os.Stat(f)
	b.SetBytes(info.Size())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		st, err := ProcessFileWithOptions(f, Options{CPUPercent: 100})
		if err != nil || st.Total != 20000 {
			b.Fatalf("total=%d err=%v", st.Total, err)
		}
	}
}

// 以下为改造前的实现（正则逐字段），仅用于对比基准
var (
	benchReTimestamp    = regexp.MustCompile(`"@timestamp":\s*"([^"]+)"`)
	benchReURI          = regexp.MustCompile(`"request_uri":\s*"([^"]*)"`)
	benchReStatus       = regexp.MustCompile(`"status":\s*"(\d+)"`)
	benchReSize         = regexp.MustCompile(`"size":\s*"(\d+)"`)
	benchReUpstreamTime = regexp.MustCompile(`"upstream_response_time":\s*"([\d.]+)"`)
	benchReResponseTime = regexp.MustCompile(`"responsetime":\s*"([\d.]+)"`)
	benchReClient       = regexp.MustCompile(`"client":\s*"([^"]*)"`)
	benchReMethod       = regexp.MustCompile(`"request_method":\s*"([^"]*)"`)
	benchReUpstreamAddr = regexp.MustCompile(`"upstream_addr":\s*"([^"]*)"`)
)

func BenchmarkParseRegex(b *testing.B) {
	data := genSample(20000)
	lines := splitLines(data)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		agg := NewAgg()
		skipped := 0
		for _, line := range lines {
			m := benchReTimestamp.FindStringSubmatch(line)
			if m == nil {
				skipped++
				continue
			}
			ts, err := time.Parse("2006-01-02T15:04:05-07:00", m[1])
			if err != nil {
				skipped++
				continue
			}
			var r record
			r.ts = ts
			r.day = ts.Format("2006-01-02")
			r.hour = ts.Hour()
			if v := benchReURI.FindStringSubmatch(line); v != nil {
				r.uri = v[1]
			}
			if v := benchReStatus.FindStringSubmatch(line); v != nil {
				r.status = v[1]
			}
			if v := benchReSize.FindStringSubmatch(line); v != nil {
				r.size, _ = strconv.ParseInt(v[1], 10, 64)
			}
			if v := benchReUpstreamTime.FindStringSubmatch(line); v != nil {
				r.upstreamTime, _ = strconv.ParseFloat(v[1], 64)
			} else if v := benchReResponseTime.FindStringSubmatch(line); v != nil {
				r.upstreamTime, _ = strconv.ParseFloat(v[1], 64)
			}
			if v := benchReClient.FindStringSubmatch(line); v != nil {
				r.client = v[1]
			}
			if v := benchReMethod.FindStringSubmatch(line); v != nil {
				r.method = v[1]
			}
			if v := benchReUpstreamAddr.FindStringSubmatch(line); v != nil {
				r.upstreamAddr = v[1]
			}
			r.isError = len(r.status) > 0 && r.status[0] >= '4'
			r.isSlow = r.upstreamTime > 1.0
			agg.add(&r)
		}
		_ = skipped
		if agg.Total != 20000 {
			b.Fatalf("total=%d", agg.Total)
		}
	}
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return out
}

// BenchmarkRealLog 用仓库里的真实日志（若存在）测吞吐
func BenchmarkRealLog(b *testing.B) {
	const p = "../../28888_access_json.log"
	info, err := os.Stat(p)
	if err != nil {
		b.Skip("未找到真实日志样本")
	}
	b.SetBytes(info.Size())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		st, err := ProcessFileWithOptions(p, Options{CPUPercent: 100, EarlyExitLines: -1})
		if err != nil || st.Total == 0 {
			b.Fatalf("total=%d err=%v", st.Total, err)
		}
	}
}

// BenchmarkHighCard 模拟真实高基数日志（大量不同 URI / 客户端 IP），
// 用于观察 map 与 GC 开销；这是"解析速度慢"的主要场景。
func BenchmarkHighCard(b *testing.B) {
	var sb strings.Builder
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(&sb, `{"@timestamp": "2026-11-01T%02d:%02d:%02d+08:00", "client": "10.%d.%d.%d", `+
			`"size": "5000", "status": "200", "upstream_response_time": "0.05", "request_method": "GET", `+
			`"request_uri": "/bbt-api/sys/file/load?id=%d", "upstream_addr": "10.0.0.%d:8080", "upstream_status": "200"}`+"\n",
			i%24, i%60, i%60, i%3, (i/3)%255, i%254, 900000000000000000+i, 11+i%3)
	}
	dir := b.TempDir()
	f := filepath.Join(dir, "high.log")
	if err := os.WriteFile(f, []byte(sb.String()), 0644); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(sb.Len()))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ProcessFileWithOptions(f, Options{}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkParseInMemory 纯内存解析基准：不碰磁盘，A/B 对比时噪声最低
func BenchmarkParseInMemory(b *testing.B) {
	data := []byte(genSample(20000))
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		st, err := ProcessReader(bytes.NewReader(data), Options{CPUPercent: 100, URINormalize: true})
		if err != nil || st.Total != 20000 {
			b.Fatalf("total=%v err=%v", st.Total, err)
		}
	}
}

// genDense 生成「每秒多行」的真实形态样本（真实站点一天 140 万行 / 86400 秒 ≈ 16 行/秒，
// 峰值可达几百行/秒）。合成数据若每秒只有 1 行，会夸大"逐秒表优化"的成本。
func genDense(n int, perSec int, uris int) string {
	var b bytes.Buffer
	for i := 0; i < n; i++ {
		sec := i / perSec
		uri := fmt.Sprintf("/bbt-api/api%d", i%uris)
		fmt.Fprintf(&b, `{"@timestamp": "2026-09-01T%02d:%02d:%02d+08:00", "client": "10.20.%d.%d", "size": "%d", "status": "%d", "upstream_response_time": "0.0%d", "request_method": "GET", "request_uri": "%s", "upstream_addr": "10.0.0.%d:9510", "upstream_status": "%d"}`+"\n",
			sec/3600%24, sec/60%60, sec%60, i%200, i%250, 500+i%900, 200+i%5, i%9, uri, i%6, 200)
	}
	return b.String()
}

// BenchmarkDenseSeconds 真实形态：每秒 200 行、300 个接口
func BenchmarkDenseSeconds(b *testing.B) {
	data := []byte(genDense(20000, 200, 300))
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		st, err := ProcessReader(bytes.NewReader(data), Options{CPUPercent: 100, URINormalize: true})
		if err != nil || st.Total != 20000 {
			b.Fatalf("total=%v err=%v", st.Total, err)
		}
	}
}

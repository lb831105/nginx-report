package staticasset

import "testing"

// 静态资源识别：后缀、路径段、大小写、query 都要覆盖，
// 字符串与 []byte 两条路径结果必须一致（日志解析走 IsBytes）。
func TestIsStaticAsset(t *testing.T) {
	cases := map[string]bool{
		"/pro/assets/user-b8d95580.svg":   true,
		"/pro/assets/reload-0c860b1f.svg": true,
		"/static/app.a1b2c3.js":           true,
		"/static/app.9f8e7d":              true, // 无扩展名但在 static 段下
		"/assets/img/logo.PNG":            true, // 大小写
		"/css/main.css?v=2":               true, // 带 query
		"/fonts/song.woff2":               true,
		"/pro/assets/SourceHanSans.woff2": true,
		"/bbt-api/org/online/heartbeat":   false,
		"/bbt-api/sys/dict/item/select":   false,
		"/":                               false,
		"/manage/index.html":              false, // html 不算静态（可能是页面/接口）
		"/bbt-api/assets/list":            false, // api 命名空间下的同名目录仍是业务接口
		"/x/y/static":                     false, // 只认前两段；第三段不参与判断
		"/verylongprefixwithAPIname/x":    false, // 超长段仍要识别 api 命名空间
		"/PRO/ASSETS/LOGO.SVG":            true,  // 大小写与长路径组合
	}
	for uri, want := range cases {
		if got := Is(uri); got != want {
			t.Errorf("Is(%q)=%v, want %v", uri, got, want)
		}
		if got := IsBytes([]byte(uri)); got != want {
			t.Errorf("IsBytes(%q)=%v, want %v", uri, got, want)
		}
		if got := IsBytes(nil); got != false {
			t.Errorf("IsBytes(nil)=%v, want false", got)
		}
	}
}

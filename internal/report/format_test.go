package report

import (
	"reflect"
	"testing"
)

func TestFormats(t *testing.T) {
	cases := []struct {
		spec string
		want []string
	}{
		{"", []string{"html"}}, // 默认 HTML
		{"html", []string{"html"}},
		{"pdf", []string{"pdf"}},
		{"xls", []string{"xlsx"}}, // xls 是 xlsx 的别名
		{"excel", []string{"xlsx"}},
		{" PDF , xlsx ", []string{"pdf", "xlsx"}}, // 大小写、空格容错
		{"all", []string{"html", "pdf", "xlsx"}},
		{"html,html,pdf", []string{"html", "pdf"}}, // 去重
		{"docx", []string{"html"}},                 // 不认识就回退默认
		{"pdf,docx", []string{"pdf"}},
	}
	for _, c := range cases {
		if got := Formats(c.spec); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Formats(%q) = %v, want %v", c.spec, got, c.want)
		}
	}
}

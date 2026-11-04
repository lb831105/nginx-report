package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.json")

	if err := WriteFileAtomic(p, []byte("v1"), 0644); err != nil {
		t.Fatal(err)
	}
	// 覆盖写
	if err := WriteFileAtomic(p, []byte("v2"), 0644); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "v2" {
		t.Fatalf("内容=%q err=%v", b, err)
	}
	// 不应残留临时文件
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("残留临时文件: %v", entries)
	}
	// 目标目录不存在时应返回错误而不是 panic
	if err := WriteFileAtomic(filepath.Join(dir, "nope", "x.html"), []byte("x"), 0644); err == nil {
		t.Error("目录不存在时应返回错误")
	}
}

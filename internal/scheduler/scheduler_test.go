package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestParseClock(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"00:10", 10 * time.Minute, false},
		{"08:00", 8 * time.Hour, false},
		{"23:59", 23*time.Hour + 59*time.Minute, false},
		{"8:5", 8*time.Hour + 5*time.Minute, false},
		{"24:00", 0, true},
		{"12:60", 0, true},
		{"abc", 0, true},
		{"", 0, true},
	}
	for _, c := range cases {
		got, err := ParseClock(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseClock(%q) 应该报错", c.in)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("ParseClock(%q)=%v,%v want %v", c.in, got, err, c.want)
		}
		if FormatClock(got) == "" {
			t.Errorf("FormatClock(%v) 为空", got)
		}
	}
}

func at(s string) time.Time {
	v, _ := time.Parse("2006-01-02 15:04", s)
	return v
}

func TestSchedulerRunsOnceAfterSchedule(t *testing.T) {
	var runs []string
	failing := false
	s := &Scheduler{Jobs: []Job{{
		Name: "daily",
		Due: func(now time.Time) (string, time.Time) {
			return "2026-11-01", at("2026-11-02 00:10")
		},
		Run: func(key string) error {
			runs = append(runs, key)
			if failing {
				return errors.New("boom")
			}
			return nil
		},
		Retry: 10 * time.Minute,
	}}}

	st := NewState()

	// 未到点不执行
	s.Step(at("2026-11-02 00:09"), st)
	if len(runs) != 0 {
		t.Fatalf("未到点不应执行, runs=%v", runs)
	}

	// 到点执行一次
	s.Step(at("2026-11-02 00:10"), st)
	if len(runs) != 1 || runs[0] != "2026-11-01" {
		t.Fatalf("到点应执行一次, runs=%v", runs)
	}

	// 同一轮不重复
	s.Step(at("2026-11-02 05:00"), st)
	if len(runs) != 1 {
		t.Fatalf("已完成的任务不应重复执行, runs=%v", runs)
	}

	// 次日窗口推进 → 再次执行
	s2 := &Scheduler{Jobs: []Job{{
		Name: "daily",
		Due: func(now time.Time) (string, time.Time) {
			day := now.AddDate(0, 0, -1).Format("2006-01-02")
			return day, at("2026-11-03 00:10")
		},
		Run: func(key string) error {
			runs = append(runs, key)
			return nil
		},
	}}}
	s2.Step(at("2026-11-03 00:10"), st)
	if len(runs) != 2 || runs[1] != "2026-11-02" {
		t.Fatalf("窗口推进后应再执行, runs=%v", runs)
	}
}

func TestSchedulerSkipsDone(t *testing.T) {
	called := false
	s := &Scheduler{Jobs: []Job{{
		Name: "weekly",
		Due: func(now time.Time) (string, time.Time) {
			return "2026-10-26|2026-11-01", at("2026-11-02 08:00")
		},
		Done: func(key string) bool { return true },
		Run:  func(key string) error { called = true; return nil },
	}}}
	s.Step(at("2026-11-02 09:00"), NewState())
	if called {
		t.Fatal("成果已存在时不应执行")
	}
}

func TestSchedulerRetry(t *testing.T) {
	var runs int
	s := &Scheduler{Jobs: []Job{{
		Name: "daily",
		Due: func(now time.Time) (string, time.Time) {
			return "2026-11-01", at("2026-11-02 00:10")
		},
		Run:   func(key string) error { runs++; return errors.New("no data") },
		Retry: 10 * time.Minute,
	}}}
	st := NewState()

	s.Step(at("2026-11-02 00:10"), st)
	if runs != 1 {
		t.Fatalf("首次应执行, runs=%d", runs)
	}
	// 重试间隔内不执行
	s.Step(at("2026-11-02 00:15"), st)
	if runs != 1 {
		t.Fatalf("重试间隔内不应执行, runs=%d", runs)
	}
	// 到重试时间再次执行
	s.Step(at("2026-11-02 00:20"), st)
	if runs != 2 {
		t.Fatalf("到重试时间应执行, runs=%d", runs)
	}
}

func TestSchedulerOnResult(t *testing.T) {
	type res struct {
		name, key string
		err       error
	}
	var got []res
	s := &Scheduler{
		Jobs: []Job{{
			Name: "daily",
			Due: func(now time.Time) (string, time.Time) {
				return "2026-11-01", at("2026-11-02 00:10")
			},
			Run: func(key string) error { return nil },
		}},
		OnResult: func(name, key string, err error) { got = append(got, res{name, key, err}) },
	}
	s.Step(at("2026-11-02 00:10"), NewState())
	if len(got) != 1 || got[0].name != "daily" || got[0].key != "2026-11-01" || got[0].err != nil {
		t.Fatalf("OnResult 回调不正确: %+v", got)
	}
}

func TestSchedulerGivesUpAfterMaxAttempts(t *testing.T) {
	var runs int
	var gaveUp struct {
		name     string
		key      string
		attempts int
	}
	s := &Scheduler{
		Jobs: []Job{{
			Name: "daily",
			Due: func(now time.Time) (string, time.Time) {
				return "2026-11-01", at("2026-11-02 00:10")
			},
			Run:         func(key string) error { runs++; return errors.New("no data") },
			Retry:       time.Minute,
			MaxAttempts: 3,
		}},
		OnGiveUp: func(name, key string, attempts int) {
			gaveUp.name, gaveUp.key, gaveUp.attempts = name, key, attempts
		},
	}
	st := NewState()
	// 第 1 次、第 2 次、第 3 次触发；每次之间需超过重试间隔
	s.Step(at("2026-11-02 00:10"), st)
	s.Step(at("2026-11-02 00:11"), st)
	s.Step(at("2026-11-02 00:12"), st)
	if runs != 3 {
		t.Fatalf("应尝试 3 次, got %d", runs)
	}
	if gaveUp.name != "daily" || gaveUp.attempts != 3 {
		t.Fatalf("应触发放弃回调: %+v", gaveUp)
	}
	// 放弃后本周期不再执行
	s.Step(at("2026-11-02 00:20"), st)
	s.Step(at("2026-11-02 12:00"), st)
	if runs != 3 {
		t.Fatalf("放弃后不应再执行, got %d", runs)
	}
}

func TestSchedulerNextCycleResetsAttempts(t *testing.T) {
	var runs int
	cur := "2026-11-01"
	s := &Scheduler{Jobs: []Job{{
		Name: "daily",
		Due: func(now time.Time) (string, time.Time) {
			return cur, at("2026-11-02 00:10")
		},
		Run:         func(key string) error { runs++; return nil },
		Retry:       time.Minute,
		MaxAttempts: 1,
	}}}
	st := NewState()
	s.Step(at("2026-11-02 00:10"), st)
	cur = "2026-11-02" // 次日
	s.Step(at("2026-11-03 00:10"), st)
	if runs != 2 {
		t.Fatalf("新周期应再次执行, got %d", runs)
	}
}

// 卡住的任务不能拖住进程退出（有上限的优雅等待）
func TestRunReturnsWhenJobBlocks(t *testing.T) {
	old := shutdownGrace
	shutdownGrace = 50 * time.Millisecond
	defer func() { shutdownGrace = old }()

	block := make(chan struct{})
	defer close(block)

	s := &Scheduler{
		Tick: time.Millisecond,
		Now:  func() time.Time { return time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC) },
		Jobs: []Job{{
			Name: "slow",
			Due:  func(time.Time) (string, time.Time) { return "k", time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC) },
			Run:  func(string) error { <-block; return nil },
		}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	time.Sleep(20 * time.Millisecond) // 确保任务已在执行
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("任务卡死时 Run 未在优雅等待上限内返回")
	}
}

// 任务在独立 goroutine 执行：较慢的任务不会阻塞检查循环（后续 tick 仍被消费）
func TestRunKeepsTickingWhileJobRuns(t *testing.T) {
	s := &Scheduler{
		Tick: 5 * time.Millisecond,
		Now:  func() time.Time { return time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC) },
	}
	var mu sync.Mutex
	runs := 0
	s.Jobs = []Job{{
		Name: "j",
		Due:  func(time.Time) (string, time.Time) { return "k", time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC) },
		Run: func(string) error {
			mu.Lock()
			runs++
			mu.Unlock()
			time.Sleep(30 * time.Millisecond)
			return nil
		},
		MaxAttempts: 0,
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if runs == 0 {
		t.Fatal("任务未执行")
	}
}

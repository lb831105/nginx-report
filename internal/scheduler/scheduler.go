// Package scheduler 提供常驻模式下的周期任务调度。
//
// 每个任务只需要描述三件事：
//   - Due: 给定当前时间，返回「本次任务的键 + 计划执行时刻」
//   - Done: 该键对应的成果是否已存在（用于重启后跳过已完成的）
//   - Run: 执行任务
//
// 调度器在每个 tick 检查一次：到了计划时刻且未完成就执行；
// 失败按任务的 Retry 间隔重试，成功则本轮不再重复执行。
package scheduler

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Job 一个周期任务
type Job struct {
	Name string
	// Due 返回本次任务键与计划执行时刻
	Due func(now time.Time) (key string, at time.Time)
	// Done 可选：返回 true 表示该键的成果已存在，跳过执行
	Done func(key string) bool
	// Run 执行任务
	Run func(key string) error
	// Retry 失败后的重试间隔，<=0 时取 10 分钟
	Retry time.Duration
	// MaxAttempts 最多尝试次数（含首次），<=0 表示不限制；超过后本周期放弃，等下一个周期
	MaxAttempts int
}

// State 调度器运行状态，由调用方持有，便于测试与持久化扩展
type State struct {
	LastDone map[string]string
	NextTry  map[string]time.Time
	Attempts map[string]int
	curKey   map[string]string
}

// NewState 创建空状态
func NewState() *State {
	return &State{
		LastDone: map[string]string{},
		NextTry:  map[string]time.Time{},
		Attempts: map[string]int{},
		curKey:   map[string]string{},
	}
}

// Scheduler 周期任务调度器
type Scheduler struct {
	Jobs []Job
	// Tick 检查间隔，<=0 时取 30 秒
	Tick time.Duration
	// Now 可注入的时钟（测试用），默认 time.Now
	Now func() time.Time
	// OnResult 每次执行后回调，err 为 nil 表示成功
	OnResult func(jobName, key string, err error)
	// OnGiveUp 重试达到上限后回调
	OnGiveUp func(jobName, key string, attempts int)
}

// DefaultRetry 默认失败重试间隔
const DefaultRetry = 10 * time.Minute

// DefaultTick 默认检查间隔
const DefaultTick = 30 * time.Second

// shutdownGrace 退出时等待「正在执行的任务」的最长时间。
// 任务内部写文件都是原子的，超时后直接退出不会留下半截文件。
var shutdownGrace = 5 * time.Second

// Run 启动调度循环，直到 ctx 被取消后返回 nil。
//
// 任务在独立 goroutine 里串行执行：既不会因为某个任务耗时长而卡住检查循环
// （漏掉其他任务的到点），也不会让两个任务同时跑起来抢 CPU（生产环境要求低负载）。
func (s *Scheduler) Run(ctx context.Context) error {
	if s.Tick <= 0 {
		s.Tick = DefaultTick
	}
	if s.Now == nil {
		s.Now = time.Now
	}

	st := NewState()

	work := make(chan time.Time, 1)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		for now := range work {
			s.Step(now, st)
		}
	}()

	// 上一轮还没跑完就跳过本次 tick（不排队堆积）
	enqueue := func(now time.Time) {
		select {
		case work <- now:
		default:
		}
	}

	// 启动时先检查一次，实现「重启后自动补跑」
	enqueue(s.Now())

	ticker := time.NewTicker(s.Tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			close(work)
			select {
			case <-workerDone:
			case <-time.After(shutdownGrace):
			}
			return nil
		case <-ticker.C:
			enqueue(s.Now())
		}
	}
}

// Step 执行一轮检查（导出以便测试与手动触发）
func (s *Scheduler) Step(now time.Time, st *State) {
	if st == nil {
		st = NewState()
	}
	for _, job := range s.Jobs {
		if t, ok := st.NextTry[job.Name]; ok && now.Before(t) {
			continue // 仍在重试等待中
		}
		key, at := job.Due(now)
		if key == "" || now.Before(at) {
			continue // 还没到点
		}
		if st.LastDone[job.Name] == key {
			continue // 本轮已完成
		}
		if st.curKey[job.Name] != key {
			// 进入新周期，重置尝试次数
			st.curKey[job.Name] = key
			st.Attempts[job.Name] = 0
		}
		if job.Done != nil && job.Done(key) {
			st.LastDone[job.Name] = key
			delete(st.NextTry, job.Name)
			continue
		}

		err := job.Run(key)
		if err != nil {
			st.Attempts[job.Name]++
			if job.MaxAttempts > 0 && st.Attempts[job.Name] >= job.MaxAttempts {
				// 放弃本轮，避免日志未切割时整天反复扫大文件；等下个周期再试
				st.LastDone[job.Name] = key
				delete(st.NextTry, job.Name)
				if s.OnGiveUp != nil {
					s.OnGiveUp(job.Name, key, st.Attempts[job.Name])
				}
			} else {
				retry := job.Retry
				if retry <= 0 {
					retry = DefaultRetry
				}
				st.NextTry[job.Name] = now.Add(retry)
			}
		} else {
			st.LastDone[job.Name] = key
			st.Attempts[job.Name] = 0
			delete(st.NextTry, job.Name)
		}
		if s.OnResult != nil {
			s.OnResult(job.Name, key, err)
		}
	}
}

// ParseClock 解析 "HH:MM" 为自当日 0 点起的偏移量
func ParseClock(s string) (time.Duration, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("时间格式错误 %q，应为 HH:MM", s)
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("时间格式错误 %q，应为 HH:MM（00:00 ~ 23:59）", s)
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute, nil
}

// AtClock 返回 day 当天 offset 时刻
func AtClock(day time.Time, offset time.Duration) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, day.Location()).Add(offset)
}

// FormatClock 将偏移量格式化为 HH:MM
func FormatClock(offset time.Duration) string {
	if offset < 0 {
		offset = 0
	}
	total := int(offset / time.Minute)
	return fmt.Sprintf("%02d:%02d", total/60%24, total%60)
}

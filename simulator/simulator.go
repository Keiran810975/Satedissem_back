// Package simulator 提供离散事件仿真引擎，仿照 ns-3 架构设计。
// 单线程 + 全局逻辑时钟 + 优先队列事件调度。
package simulator

import (
	"container/heap"
	"fmt"
)

// Time 仿真时间，单位：纳秒
type Time uint64

// 时间常量，方便构造
const (
	Nanosecond  Time = 1
	Microsecond Time = 1000
	Millisecond Time = 1000 * Microsecond
	Second      Time = 1000 * Millisecond
)

// FormatTime 将纳秒时间格式化为人类可读字符串
func FormatTime(t Time) string {
	if t >= Second {
		return fmt.Sprintf("%.6f s", float64(t)/float64(Second))
	}
	if t >= Millisecond {
		return fmt.Sprintf("%.3f ms", float64(t)/float64(Millisecond))
	}
	if t >= Microsecond {
		return fmt.Sprintf("%.3f µs", float64(t)/float64(Microsecond))
	}
	return fmt.Sprintf("%d ns", t)
}

// EventCallback 是事件触发时执行的回调
type EventCallback func()

// Event 表示一个离散事件
type Event struct {
	Time Time          // 事件触发时刻
	ID   uint64        // 全局唯一 ID，用于同时刻事件之间的稳定排序
	Fn   EventCallback // 回调函数
	Tag  string        // 可选的调试标签
}

// Simulator 是离散事件仿真引擎
type Simulator struct {
	Now        Time       // 当前仿真时间
	queue      EventQueue // 事件优先队列
	nextID     uint64     // 下一个事件 ID
	eventCount uint64     // 已处理事件总数
	stopped    bool       // 外部停止标志

	// 统计回调：每处理 N 个事件触发一次
	ProgressInterval uint64
	OnProgress       func(sim *Simulator)
}

// New 创建一个新的仿真器
func New() *Simulator {
	s := &Simulator{}
	heap.Init(&s.queue)
	return s
}

// Schedule 在绝对时刻 t 安排事件
func (s *Simulator) Schedule(t Time, fn EventCallback) uint64 {
	return s.ScheduleWithTag(t, fn, "")
}

// ScheduleWithTag 在绝对时刻 t 安排事件，并附带调试标签
func (s *Simulator) ScheduleWithTag(t Time, fn EventCallback, tag string) uint64 {
	if t < s.Now {
		panic(fmt.Sprintf("simulator: cannot schedule event in the past (now=%d, scheduled=%d, tag=%s)", s.Now, t, tag))
	}
	e := Event{
		Time: t,
		ID:   s.nextID,
		Fn:   fn,
		Tag:  tag,
	}
	s.nextID++
	heap.Push(&s.queue, e)
	return e.ID
}

// ScheduleDelay 在当前时间 + delay 后安排事件
func (s *Simulator) ScheduleDelay(delay Time, fn EventCallback) uint64 {
	return s.Schedule(s.Now+delay, fn)
}

// ScheduleDelayWithTag 在当前时间 + delay 后安排事件，附带调试标签
func (s *Simulator) ScheduleDelayWithTag(delay Time, fn EventCallback, tag string) uint64 {
	return s.ScheduleWithTag(s.Now+delay, fn, tag)
}

// Run 启动仿真循环，直到事件队列为空或被 Stop() 停止
func (s *Simulator) Run() {
	s.stopped = false
	for s.queue.Len() > 0 && !s.stopped {
		e := heap.Pop(&s.queue).(Event)
		s.Now = e.Time
		e.Fn()
		s.eventCount++

		if s.ProgressInterval > 0 && s.OnProgress != nil && s.eventCount%s.ProgressInterval == 0 {
			s.OnProgress(s)
		}
	}
}

// Stop 终止仿真循环
func (s *Simulator) Stop() {
	s.stopped = true
}

// EventCount 返回已处理的事件总数
func (s *Simulator) EventCount() uint64 {
	return s.eventCount
}

// PendingEvents 返回队列中待处理的事件数
func (s *Simulator) PendingEvents() int {
	return s.queue.Len()
}

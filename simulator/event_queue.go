package simulator

// EventQueue 实现 container/heap.Interface 的最小堆
// 排序规则：Time 小优先；Time 相同时 ID 小优先（保证 FIFO 稳定性）
type EventQueue []Event

func (q EventQueue) Len() int { return len(q) }

func (q EventQueue) Less(i, j int) bool {
	if q[i].Time == q[j].Time {
		return q[i].ID < q[j].ID
	}
	return q[i].Time < q[j].Time
}

func (q EventQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }

func (q *EventQueue) Push(x interface{}) {
	*q = append(*q, x.(Event))
}

func (q *EventQueue) Pop() interface{} {
	old := *q
	n := len(old)
	e := old[n-1]
	*q = old[:n-1]
	return e
}

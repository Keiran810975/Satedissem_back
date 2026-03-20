package protocol

import "sat-sim/simulator"

// =============================================================================
// FIFOChannel — 先进先出信道（默认）
//
// 与原来 Link.nextAvailable 逻辑完全等价：
//   严格排队，无竞争冲突，包一个接一个发送。
// =============================================================================

// FIFOChannel 先进先出信道：严格按序，无需介质争用。
type FIFOChannel struct {
	nextAvailable simulator.Time
}

// NewFIFOChannel 创建一个 FIFO 信道实例。
func NewFIFOChannel() *FIFOChannel {
	return &FIFOChannel{}
}

func (f *FIFOChannel) RequestAccess(now simulator.Time, _ int) simulator.Time {
	if now >= f.nextAvailable {
		return now
	}
	return f.nextAvailable
}

func (f *FIFOChannel) CommitTransmit(_, txEnd simulator.Time) {
	f.nextAvailable = txEnd
}

func (f *FIFOChannel) ResetWindow(windowStart simulator.Time) {
	// 新窗口开始，清空跨窗口的排队积压
	f.nextAvailable = windowStart
}

// BusyUntil 返回当前信道已排队发送的忙碌截止时刻。
func (f *FIFOChannel) BusyUntil() simulator.Time {
	return f.nextAvailable
}

// =============================================================================
// TDMAChannel — 时分多址信道（TDMA）
//
// 将每个联系窗口按固定时隙长度切分，该链路只能在分配到的时隙内发包。
// 适用于卫星间有严格时隙调度协议的场景。
//
// 用法示例（每 10ms 一个时隙，本链路占第 0 个时隙）：
//
//	ch := protocol.NewTDMAChannel(
//	    10*simulator.Millisecond, // 帧长
//	    0,                        // 本链路的时隙偏移（帧内起始偏移）
//	    5*simulator.Millisecond,  // 时隙宽度（可发送时间）
//	)
// =============================================================================

// TDMAChannel 时分多址信道：只在分配的时隙内发送。
type TDMAChannel struct {
	frameLen    simulator.Time // 一帧的长度（时隙周期）
	slotOffset  simulator.Time // 本链路在帧内的起始偏移
	slotWidth   simulator.Time // 本链路的时隙宽度
	windowStart simulator.Time // 当前联系窗口的起始时间
}

// NewTDMAChannel 创建一个 TDMA 信道。
//
//	frameLen   — 帧长（例如 10ms）
//	slotOffset — 本链路在帧内的偏移（例如 0ms, 2ms, 4ms…分别对应不同链路）
//	slotWidth  — 每帧内可发时长（例如 2ms）
func NewTDMAChannel(frameLen, slotOffset, slotWidth simulator.Time) *TDMAChannel {
	return &TDMAChannel{
		frameLen:   frameLen,
		slotOffset: slotOffset,
		slotWidth:  slotWidth,
	}
}

func (t *TDMAChannel) RequestAccess(now simulator.Time, _ int) simulator.Time {
	// 在 [now, ∞) 中找到第一个属于本时隙的时刻
	relNow := now - t.windowStart
	frameIdx := relNow / t.frameLen
	// 当前帧的时隙起始
	slotStart := t.windowStart + frameIdx*t.frameLen + t.slotOffset
	if slotStart < now {
		// 本帧时隙已过，等下一帧
		slotStart += t.frameLen
	}
	return slotStart
}

func (t *TDMAChannel) CommitTransmit(_, _ simulator.Time) {
	// TDMA 不需要额外状态：每帧时隙固定，不受占用影响
}

func (t *TDMAChannel) ResetWindow(windowStart simulator.Time) {
	t.windowStart = windowStart
}

// BusyUntil 对固定时隙 TDMA 返回窗口起点，表示不维护 FIFO 队列式忙碌时刻。
func (t *TDMAChannel) BusyUntil() simulator.Time {
	return t.windowStart
}

// =============================================================================
// PushAllBaseSat — 默认基站注入策略
//
// 联系窗口开启时，将基站持有而卫星缺少的所有分片推送过去。
// 与原来 dynamic.go 中的 injectBase 函数行为完全等价。
// =============================================================================

// PushAllBaseSat 推送卫星缺失的全部分片（默认基站注入策略）。
type PushAllBaseSat struct{}

// DefaultBaseSat 是 PushAllBaseSat 的共享实例，供默认配置直接使用。
var DefaultBaseSat BaseSatStrategy = &PushAllBaseSat{}

func (p *PushAllBaseSat) OnBaseLinkUp(base NodeInfo, sat NodeInfo, link LinkInfo, fragmentSize int) {
	if base.GetStorage() == nil || sat.GetStorage() == nil {
		return
	}
	for _, fragID := range base.GetStorage().OwnedFragments() {
		if !sat.GetStorage().Has(fragID) {
			link.TransmitPacket(Packet{
				FragmentID: fragID,
				Size:       fragmentSize,
				SrcID:      base.NodeID(),
				DstID:      sat.NodeID(),
			})
		}
	}
}

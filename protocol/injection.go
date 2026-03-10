package protocol

// -------------------------------------------------------------------
// RoundRobinInjection — 轮询注入策略
// 基站将 M 个分片依次分配给 N 个卫星，第 i 个分片→卫星 i%N
// -------------------------------------------------------------------

// RoundRobinInjection 按轮询方式将分片注入各卫星
type RoundRobinInjection struct{}

func NewRoundRobinInjection() *RoundRobinInjection {
	return &RoundRobinInjection{}
}

func (r *RoundRobinInjection) Inject(base NodeInfo, satellites []NodeInfo, fragments []int, fragmentSize int) {
	n := len(satellites)
	if n == 0 {
		return
	}

	links := base.GetLinks()

	for _, fragID := range fragments {
		targetIdx := fragID % n
		target := satellites[targetIdx]

		// 找到基站到目标卫星的链路
		var link LinkInfo
		for _, l := range links {
			if l.Destination().NodeID() == target.NodeID() {
				link = l
				break
			}
		}
		if link == nil {
			continue
		}

		pkt := Packet{
			FragmentID: fragID,
			Size:       fragmentSize,
			SrcID:      base.NodeID(),
			DstID:      target.NodeID(),
			HopCount:   0,
		}

		link.TransmitPacket(pkt)
	}
}

// -------------------------------------------------------------------
// SpreadInjection — 均匀分散注入策略
// 尽量让每个卫星初始获得不同的分片，减少初始冗余
// -------------------------------------------------------------------

// SpreadInjection 均匀分散注入
type SpreadInjection struct{}

func NewSpreadInjection() *SpreadInjection {
	return &SpreadInjection{}
}

func (s *SpreadInjection) Inject(base NodeInfo, satellites []NodeInfo, fragments []int, fragmentSize int) {
	// 和 RoundRobin 行为相同，但可以扩展为更复杂的策略
	rr := &RoundRobinInjection{}
	rr.Inject(base, satellites, fragments, fragmentSize)
}

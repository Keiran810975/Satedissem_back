package protocol

import (
	"fmt"
	"math"
	"math/rand"
	"sat-sim/simulator"
)

const rlncSymbolIDBase = 1_000_000_000

type rlncNodeState struct {
	rank               int
	decoded            bool
	decodingInProgress bool
	nextSymbolSeq      int
	lastSourceID       int
	decodeDoneAt       simulator.Time
	hasDecodeDoneAt    bool
	codedSent          int
	codedRecv          int
}

type RLNCNodeStats struct {
	NodeID      int
	Rank        int
	Decoded     bool
	DecodeTime  simulator.Time
	CodedSent   int
	CodedRecv   int
	HasDecodeAt bool
}

type RLNCStats struct {
	ComplexityModel string
	DecodeUnitUs    float64
	SymbolBurst     int
	Nodes           []RLNCNodeStats
}

// RLNCScheduler 实现 RLNC 传播与解码流程。
// 通过解码延迟显式建模 O(K^3 + K^2L) 复杂度。
type RLNCScheduler struct {
	TotalFragments int
	FragmentSize   int
	Seed           int64
	DecodeUnitUs   float64
	SymbolBurst    int
	ForwardFanout  int

	rng    *rand.Rand
	states map[int]*rlncNodeState
	maxNodeID int
}

func NewRLNCScheduler(totalFragments, fragmentSize int, seed int64, decodeUnitUs float64, symbolBurst int) *RLNCScheduler {
	if seed == 0 {
		seed = 42
	}
	if decodeUnitUs <= 0 {
		decodeUnitUs = 4.0
	}
	if symbolBurst <= 0 {
		symbolBurst = totalFragments
		if symbolBurst < 8 {
			symbolBurst = 8
		}
	}
	return &RLNCScheduler{
		TotalFragments: totalFragments,
		FragmentSize:   fragmentSize,
		Seed:           seed,
		DecodeUnitUs:   decodeUnitUs,
		SymbolBurst:    symbolBurst,
		ForwardFanout:  0,
		rng:            rand.New(rand.NewSource(seed)),
		states:         make(map[int]*rlncNodeState),
	}
}

func (r *RLNCScheduler) OnReceive(_ NodeInfo, _ Packet) {
	// RLNC 的传播主要基于编码包（PacketRLNCCoded）。
	// 已解码出的原始分片由 InjectDecodedFragment 注入，仅用于统计和完成判定。
}

func (r *RLNCScheduler) OnTick(_ NodeInfo) {}

func (r *RLNCScheduler) OnLinkUp(node NodeInfo, link LinkInfo) {
	r.observeNode(node)
	state := r.stateOf(node)
	if !(node.IsBaseStation() || state.rank > 0 || state.decoded) {
		return
	}

	r.transmitCodedSymbol(node, link, node.NodeID(), 0)
}

func (r *RLNCScheduler) OnMetadata(node NodeInfo, pkt Packet, _ LinkInfo) {
	r.observeNode(node)
	if pkt.Type != PacketRLNCCoded {
		return
	}

	state := r.stateOf(node)
	state.codedRecv++
	state.lastSourceID = pkt.SrcID

	if node.IsBaseStation() {
		return
	}

	if !state.decoded && state.rank < r.TotalFragments {
		// 使用高概率线性无关近似：rank 越高，创新概率越低。
		innovProb := 1.0 - math.Pow(0.5, float64(r.TotalFragments-state.rank))
		if r.rng.Float64() < innovProb {
			state.rank++
		}
	}

	r.forwardCodedToNeighbors(node, pkt)

	if state.decoded || state.decodingInProgress || state.rank < r.TotalFragments {
		return
	}

	state.decodingInProgress = true
	decodeDelay := r.decodeCostDelay()
	nodeID := node.NodeID()
	srcID := state.lastSourceID
	if srcID == 0 {
		srcID = nodeID
	}

	node.GetSim().ScheduleDelayWithTag(decodeDelay, func() {
		st := r.stateOf(node)
		if st.decoded {
			return
		}
		st.decoded = true
		st.decodingInProgress = false
		st.decodeDoneAt = node.GetSim().Now
		st.hasDecodeDoneAt = true

		reconstructPerFrag := r.reconstructPerFragmentDelay()
		for fragID := 0; fragID < r.TotalFragments; fragID++ {
			id := fragID
			if reconstructPerFrag <= 0 {
				node.InjectDecodedFragment(id, srcID)
				continue
			}
			node.GetSim().ScheduleDelayWithTag(simulator.Time(fragID+1)*reconstructPerFrag, func() {
				node.InjectDecodedFragment(id, srcID)
			}, "rlnc-reconstruct")
		}
	}, fmt.Sprintf("rlnc-decode:node=%d", nodeID))
}

func (r *RLNCScheduler) stateOf(node NodeInfo) *rlncNodeState {
	id := node.NodeID()
	state, ok := r.states[id]
	if ok {
		return state
	}

	state = &rlncNodeState{}
	if node.IsBaseStation() {
		state.rank = r.TotalFragments
		state.decoded = true
	}
	r.states[id] = state
	return state
}

func (r *RLNCScheduler) forwardCodedToNeighbors(node NodeInfo, pkt Packet) {
	state := r.stateOf(node)
	if !(state.rank > 0 || state.decoded) {
		return
	}

	candidates := make([]LinkInfo, 0, len(node.GetLinks()))
	for _, link := range node.GetLinks() {
		dst := link.Destination().NodeID()
		if dst == pkt.SrcID {
			continue
		}
		candidates = append(candidates, link)
	}

	if len(candidates) == 0 {
		return
	}

	fanout := r.ForwardFanout
	if fanout <= 0 {
		fanout = 2 + r.TotalFragments/20
		if fanout < 3 {
			fanout = 3
		}
	}
	if fanout > len(candidates) {
		fanout = len(candidates)
	}

	for i := 0; i < fanout; i++ {
		j := i + r.rng.Intn(len(candidates)-i)
		candidates[i], candidates[j] = candidates[j], candidates[i]
		r.transmitCodedSymbol(node, candidates[i], node.NodeID(), pkt.HopCount+1)
	}
}

func (r *RLNCScheduler) transmitCodedSymbol(node NodeInfo, link LinkInfo, srcID int, hopCount int) {
	state := r.stateOf(node)
	symbolID := rlncSymbolIDBase + node.NodeID()*1_000_000 + state.nextSymbolSeq
	state.nextSymbolSeq++
	state.codedSent++
	pkt := Packet{
		Type:       PacketRLNCCoded,
		FragmentID: symbolID,
		Size:       r.FragmentSize,
		SrcID:      srcID,
		DstID:      link.Destination().NodeID(),
		HopCount:   hopCount,
	}

	delay := r.encodeCostDelay()
	if delay <= 0 {
		link.TransmitPacket(pkt)
		return
	}
	node.GetSim().ScheduleDelayWithTag(delay, func() {
		link.TransmitPacket(pkt)
	}, "rlnc-encode")
}

func (r *RLNCScheduler) decodeCostDelay() simulator.Time {
	k := float64(r.TotalFragments)
	lBytes := float64(r.FragmentSize)

	// 使用软化的 O(K^3 + K^2L) 近似，并保留大 K 时的显著增长。
	opUnits := (k*k*k)/(k+16.0) + (k*k*(lBytes/1024.0))/32.0

	topoScale := 1.0
	if r.maxNodeID > 0 {
		norm := float64(r.maxNodeID) / 384.0
		if norm < 1.0 {
			norm = 1.0
		}
		topoScale = 0.95 + 0.55*math.Sqrt(norm)
	}
	fragScale := 1.0 + 0.01*k
	opUnits *= topoScale * fragScale

	return simulator.Time(opUnits * r.DecodeUnitUs * float64(simulator.Microsecond))
}

func (r *RLNCScheduler) encodeCostDelay() simulator.Time {
	k := float64(r.TotalFragments)
	lBytes := float64(r.FragmentSize)

	// 编码端开销：近似 O(K^2 + KL)
	opUnits := k*k + k*(lBytes/1024.0)
	encodeUnitUs := 0.02
	return simulator.Time(opUnits * encodeUnitUs * float64(simulator.Microsecond))
}

func (r *RLNCScheduler) reconstructPerFragmentDelay() simulator.Time {
	k := float64(r.TotalFragments)
	norm := 1.0
	topoScale := 1.0
	if r.maxNodeID > 0 {
		norm = float64(r.maxNodeID) / 384.0
		if norm < 1.0 {
			norm = 1.0
		}
		topoScale = 0.7 + 0.8*math.Sqrt(norm)
	}
	perFragUs := (35000.0 + 600.0*k) * topoScale
	if k >= 60.0 {
		perFragUs *= 1.0 + 0.6*(norm-1.0)
	} else {
		perFragUs *= 1.2
	}
	perFragUs *= rlncCalibrationFactor(r.TotalFragments, norm)
	return simulator.Time(perFragUs * float64(simulator.Microsecond))
}

func rlncCalibrationFactor(fragments int, norm float64) float64 {
	if fragments >= 60 {
		if norm < 1.2 {
			return 0.95
		}
		if norm < 1.8 {
			return 1.65
		}
		if norm < 2.3 {
			return 1.30
		}
		return 1.25
	}

	if norm < 1.2 {
		return 1.30
	}
	if norm < 1.8 {
		return 1.25
	}
	if norm < 2.3 {
		return 1.35
	}
	return 1.20
}

func (r *RLNCScheduler) observeNode(node NodeInfo) {
	id := node.NodeID()
	if id > r.maxNodeID {
		r.maxNodeID = id
	}
}

// RLNCBaseSat 在 base-sat 联系窗口开启时发送编码包。
type RLNCBaseSat struct {
	totalFragments int
	fragmentSize   int
	symbolBurst    int
	nextSymbolSeq  int
}

func NewRLNCBaseSat(totalFragments, fragmentSize int) *RLNCBaseSat {
	return NewRLNCBaseSatWithBurst(totalFragments, fragmentSize, 0)
}

func NewRLNCBaseSatWithBurst(totalFragments, fragmentSize, symbolBurst int) *RLNCBaseSat {
	burst := symbolBurst
	if burst <= 0 {
		burst = totalFragments
		if burst < 8 {
			burst = 8
		}
	}
	return &RLNCBaseSat{
		totalFragments: totalFragments,
		fragmentSize:   fragmentSize,
		symbolBurst:    burst,
	}
}

func (b *RLNCBaseSat) SymbolBurst() int {
	return b.symbolBurst
}

func (b *RLNCBaseSat) OnBaseLinkUp(base NodeInfo, sat NodeInfo, link LinkInfo, _ int) {
	for i := 0; i < b.symbolBurst; i++ {
		symbolID := rlncSymbolIDBase + base.NodeID()*1_000_000 + b.nextSymbolSeq
		b.nextSymbolSeq++
		link.TransmitPacket(Packet{
			Type:       PacketRLNCCoded,
			FragmentID: symbolID,
			Size:       b.fragmentSize,
			SrcID:      base.NodeID(),
			DstID:      sat.NodeID(),
		})
	}
}

func (r *RLNCScheduler) Stats() RLNCStats {
	nodes := make([]RLNCNodeStats, 0, len(r.states))
	for id, st := range r.states {
		nodes = append(nodes, RLNCNodeStats{
			NodeID:      id,
			Rank:        st.rank,
			Decoded:     st.decoded,
			DecodeTime:  st.decodeDoneAt,
			CodedSent:   st.codedSent,
			CodedRecv:   st.codedRecv,
			HasDecodeAt: st.hasDecodeDoneAt,
		})
	}

	for i := 1; i < len(nodes); i++ {
		for j := i; j > 0 && nodes[j].NodeID < nodes[j-1].NodeID; j-- {
			nodes[j], nodes[j-1] = nodes[j-1], nodes[j]
		}
	}

	return RLNCStats{
		ComplexityModel: "O(K^3 + K^2L)",
		DecodeUnitUs:    r.DecodeUnitUs,
		SymbolBurst:     r.SymbolBurst,
		Nodes:           nodes,
	}
}

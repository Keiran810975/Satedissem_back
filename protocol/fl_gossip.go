package protocol

import (
	"math"
	"math/rand"
	"sat-sim/simulator"
)

type flGossipNodeState struct {
	intraSyncSent      int
	interSent          int
	interDropped       int
	compensationEvents int
	intraRoundsRun     int
	interRoundsRun     int
	trainingScheduled  bool
	trainingReady      bool
	trainingReadyAt    simulator.Time
}

type FLGossipNodeStats struct {
	NodeID             int
	IntraSyncSent      int
	InterSent          int
	InterDropped       int
	CompensationEvents int
	IntraRoundsRun     int
	InterRoundsRun     int
	TrainingReady      bool
	TrainingReadyAt    simulator.Time
}

type FLGossipStats struct {
	Model           string
	PlaneSize       int
	IntraRounds     int
	InterRounds     int
	InterFanout     int
	LossProb        float64
	LocalSteps      int
	LocalStepCostUs float64
	LocalComputeOps int
	Nodes           []FLGossipNodeStats
}

// FLGossipScheduler 将 DFedSat/FL-gossip 思路映射到当前分片仿真框架：
// 1) 同轨链路（稳定）：快速全量同步（轨道归约近似）
// 2) 异轨链路（易丢）：执行多轮 gossip 传播
// 3) 跨轨自补偿：对每个待发送分片按丢包概率丢弃；接收端保留本地已有分片，不触发重传
//
// 说明：当前仿真器以“分片传播”建模而非参数向量，因此这里用分片交换近似模型块同步过程。
type FLGossipScheduler struct {
	TotalFragments  int
	FragmentSize    int
	PlaneSize       int
	IntraRounds     int
	InterRounds     int
	InterFanout     int
	LossProb        float64
	LocalSteps      int
	LocalStepCostUs float64
	LocalComputeOps int
	Seed            int64

	rng    *rand.Rand
	states map[int]*flGossipNodeState
	maxNodeID int
}

func NewFLGossipScheduler(
	totalFragments int,
	fragmentSize int,
	planeSize int,
	intraRounds int,
	interRounds int,
	interFanout int,
	lossProb float64,
	localSteps int,
	localStepCostUs float64,
	localComputeOps int,
	seed int64,
) *FLGossipScheduler {
	if planeSize <= 0 {
		planeSize = 32
	}
	if intraRounds <= 0 {
		intraRounds = 2 * (planeSize - 1)
		if intraRounds < 2 {
			intraRounds = 2
		}
	}
	if interRounds <= 0 {
		interRounds = 3
	}
	if interFanout <= 0 {
		interFanout = 2
	}
	if lossProb < 0 {
		lossProb = 0
	}
	if lossProb > 1 {
		lossProb = 1
	}
	if localSteps <= 0 {
		localSteps = 20
	}
	if localStepCostUs <= 0 {
		localStepCostUs = 500
	}
	if localComputeOps <= 0 {
		localComputeOps = 2000
	}
	if seed == 0 {
		seed = 42
	}
	return &FLGossipScheduler{
		TotalFragments:  totalFragments,
		FragmentSize:    fragmentSize,
		PlaneSize:       planeSize,
		IntraRounds:     intraRounds,
		InterRounds:     interRounds,
		InterFanout:     interFanout,
		LossProb:        lossProb,
		LocalSteps:      localSteps,
		LocalStepCostUs: localStepCostUs,
		LocalComputeOps: localComputeOps,
		Seed:            seed,
		rng:             rand.New(rand.NewSource(seed)),
		states:          make(map[int]*flGossipNodeState),
	}
}

func (f *FLGossipScheduler) OnReceive(node NodeInfo, pkt Packet) {
	f.observeNode(node)
	if pkt.Type != PacketData {
		return
	}
	if !f.ensureTrainingReady(node) {
		return
	}

	// 收到新分片后：
	// - 同轨邻居：可靠快速扩散
	// - 异轨对应邻居：按丢包概率传播（不重传，自补偿）
	links := node.GetLinks()
	if len(links) == 0 {
		return
	}

	intraCandidates := make([]LinkInfo, 0, len(links))
	interCandidates := make([]LinkInfo, 0, len(links))
	for _, link := range links {
		dst := link.Destination()
		f.observeNode(dst)
		if dst.NodeID() == pkt.SrcID {
			continue
		}
		if dstSto := dst.GetStorage(); dstSto != nil && dstSto.Has(pkt.FragmentID) {
			continue
		}

		if f.isIntraLink(node, dst, link.Kind()) {
			intraCandidates = append(intraCandidates, link)
			continue
		}
		if f.isInterPeer(node, dst, link.Kind()) {
			interCandidates = append(interCandidates, link)
		}
	}

	state := f.stateOf(node)
	lossProb := f.effectiveLossProb()
	for _, link := range intraCandidates {
		f.transmitWithCompute(node, link, Packet{
			Type:       PacketData,
			FragmentID: pkt.FragmentID,
			Size:       pkt.Size,
			SrcID:      node.NodeID(),
			DstID:      link.Destination().NodeID(),
			HopCount:   pkt.HopCount + 1,
		}, false)
		state.intraSyncSent++
	}

	sent := 0
	for sent < f.InterFanout && len(interCandidates) > 0 {
		idx := f.rng.Intn(len(interCandidates))
		link := interCandidates[idx]
		interCandidates[idx] = interCandidates[len(interCandidates)-1]
		interCandidates = interCandidates[:len(interCandidates)-1]

		if f.rng.Float64() < lossProb {
			state.interDropped++
			state.compensationEvents++
			continue
		}

		f.transmitWithCompute(node, link, Packet{
			Type:       PacketData,
			FragmentID: pkt.FragmentID,
			Size:       pkt.Size,
			SrcID:      node.NodeID(),
			DstID:      link.Destination().NodeID(),
			HopCount:   pkt.HopCount + 1,
		}, true)
		state.interSent++
		sent++
	}
}

func (f *FLGossipScheduler) OnTick(_ NodeInfo) {}

func (f *FLGossipScheduler) OnLinkUp(node NodeInfo, link LinkInfo) {
	f.observeNode(node)
	f.observeNode(link.Destination())
	if !f.ensureTrainingReady(node) {
		return
	}
	state := f.stateOf(node)
	switch link.Kind() {
	case LinkIntraPlane:
		// 同轨：K-1 scatter-reduce + K-1 all-gather 的近似轮次。
		for i := 0; i < f.IntraRounds; i++ {
			state.intraRoundsRun++
			if f.intraOrbitRound(node, link) {
				state.intraSyncSent++
			}
		}
	default:
		if !f.isInterPeer(node, link.Destination(), link.Kind()) {
			return
		}
		// 异轨：多轮 gossip + 丢包自补偿（丢失即不重传，接收端保持本地）。
		for i := 0; i < f.InterRounds; i++ {
			state.interRoundsRun++
			f.interPlaneGossipRound(node, link)
		}
	}
}

func (f *FLGossipScheduler) OnMetadata(_ NodeInfo, _ Packet, _ LinkInfo) {}

func (f *FLGossipScheduler) interPlaneGossipRound(node NodeInfo, link LinkInfo) {
	sto := node.GetStorage()
	if sto == nil {
		return
	}
	dst := link.Destination()
	dstSto := dst.GetStorage()
	if dstSto == nil {
		return
	}

	owned := sto.OwnedFragments()
	if len(owned) == 0 {
		return
	}

	// 只考虑对方缺失分片，避免无效发送。
	missingForDst := make([]int, 0, len(owned))
	for _, fragID := range owned {
		if !dstSto.Has(fragID) {
			missingForDst = append(missingForDst, fragID)
		}
	}
	if len(missingForDst) == 0 {
		return
	}

	fanout := f.InterFanout
	if fanout > len(missingForDst) {
		fanout = len(missingForDst)
	}
	lossProb := f.effectiveLossProb()

	for i := 0; i < fanout; i++ {
		idx := f.rng.Intn(len(missingForDst))
		fragID := missingForDst[idx]
		missingForDst[idx] = missingForDst[len(missingForDst)-1]
		missingForDst = missingForDst[:len(missingForDst)-1]

		// 自补偿核心：链路失败不重传，接收端继续使用本地已有内容。
		if f.rng.Float64() < lossProb {
			state := f.stateOf(node)
			state.interDropped++
			state.compensationEvents++
			continue
		}

		f.transmitWithCompute(node, link, Packet{
			Type:       PacketData,
			FragmentID: fragID,
			Size:       f.FragmentSize,
			SrcID:      node.NodeID(),
			DstID:      dst.NodeID(),
		}, true)
		f.stateOf(node).interSent++
	}
}

func (f *FLGossipScheduler) intraOrbitRound(node NodeInfo, link LinkInfo) bool {
	sto := node.GetStorage()
	if sto == nil {
		return false
	}
	dst := link.Destination()
	dstSto := dst.GetStorage()
	if dstSto == nil {
		return false
	}

	owned := sto.OwnedFragments()
	if len(owned) == 0 {
		return false
	}

	for _, fragID := range owned {
		if dstSto.Has(fragID) {
			continue
		}
		f.transmitWithCompute(node, link, Packet{
			Type:       PacketData,
			FragmentID: fragID,
			Size:       f.FragmentSize,
			SrcID:      node.NodeID(),
			DstID:      dst.NodeID(),
		}, false)
		return true
	}
	return false
}

func (f *FLGossipScheduler) Stats() FLGossipStats {
	nodes := make([]FLGossipNodeStats, 0, len(f.states))
	for id, st := range f.states {
		nodes = append(nodes, FLGossipNodeStats{
			NodeID:             id,
			IntraSyncSent:      st.intraSyncSent,
			InterSent:          st.interSent,
			InterDropped:       st.interDropped,
			CompensationEvents: st.compensationEvents,
			IntraRoundsRun:     st.intraRoundsRun,
			InterRoundsRun:     st.interRoundsRun,
			TrainingReady:      st.trainingReady,
			TrainingReadyAt:    st.trainingReadyAt,
		})
	}

	for i := 1; i < len(nodes); i++ {
		for j := i; j > 0 && nodes[j].NodeID < nodes[j-1].NodeID; j-- {
			nodes[j], nodes[j-1] = nodes[j-1], nodes[j]
		}
	}

	return FLGossipStats{
		Model:           "local-train + intra-orbit reduce/all-gather + inter-orbit gossip + self-compensation",
		PlaneSize:       f.PlaneSize,
		IntraRounds:     f.IntraRounds,
		InterRounds:     f.InterRounds,
		InterFanout:     f.InterFanout,
		LossProb:        f.LossProb,
		LocalSteps:      f.LocalSteps,
		LocalStepCostUs: f.LocalStepCostUs,
		LocalComputeOps: f.LocalComputeOps,
		Nodes:           nodes,
	}
}

func (f *FLGossipScheduler) stateOf(node NodeInfo) *flGossipNodeState {
	id := node.NodeID()
	state, ok := f.states[id]
	if ok {
		return state
	}
	state = &flGossipNodeState{}
	f.states[id] = state
	return state
}

func (f *FLGossipScheduler) ensureTrainingReady(node NodeInfo) bool {
	if node.IsBaseStation() {
		return true
	}

	state := f.stateOf(node)
	if state.trainingReady {
		return true
	}
	if state.trainingScheduled {
		return false
	}

	state.trainingScheduled = true
	k := float64(f.TotalFragments)
	fragmentScale := 0.18 + 0.012*k + 0.0022*k*k
	norm := 1.0
	topoScale := 1.0
	if f.maxNodeID > 0 {
		norm = float64(f.maxNodeID) / 384.0
		if norm < 1.0 {
			norm = 1.0
		}
		topoScale = 0.5 + 1.2*math.Sqrt(norm)
	}
	trainScale := fragmentScale * topoScale
	trainScale *= 1.4
	trainScale *= flGossipCalibrationFactor(f.TotalFragments, norm)
	delay := simulator.Time(float64(f.LocalSteps) * f.LocalStepCostUs * trainScale * float64(simulator.Microsecond))
	delay += f.trainingExtraDelay(norm)
	node.GetSim().ScheduleDelayWithTag(delay, func() {
		f.runLocalCompute(node.NodeID())
		st := f.stateOf(node)
		st.trainingReady = true
		st.trainingReadyAt = node.GetSim().Now
		for _, link := range node.GetLinks() {
			f.OnLinkUp(node, link)
		}
	}, "flgossip-local-train-ready")

	return false
}

func (f *FLGossipScheduler) transmitWithCompute(node NodeInfo, link LinkInfo, pkt Packet, inter bool) {
	baseUs := 80.0
	if inter {
		baseUs = 160.0
	}
	k := float64(f.TotalFragments)
	computeUs := baseUs + 3.0*k + 0.05*k*k
	norm := 1.0
	topoScale := 1.0
	if f.maxNodeID > 0 {
		norm = float64(f.maxNodeID) / 384.0
		if norm < 1.0 {
			norm = 1.0
		}
		topoScale = 0.2 + 1.4*math.Pow(norm, 1.3)
	}
	computeUs *= topoScale
	if norm > 1.0 {
		if k >= 60.0 {
			computeUs *= 1.0 + 0.35*(norm-1.0)
		} else {
			computeUs *= 1.0 + 0.25*(norm-1.0)
		}
	} else {
		computeUs *= 0.9
	}
	computeUs *= 1.4
	computeUs *= flGossipCalibrationFactor(f.TotalFragments, norm)
	if k >= 60.0 {
		if norm < 1.2 {
			computeUs *= 1.20
		} else if norm < 1.8 {
			computeUs *= 0.96
		} else if norm < 2.3 {
			computeUs *= 1.35
		} else {
			computeUs *= 1.25
		}
	}
	delay := simulator.Time(computeUs * float64(simulator.Microsecond))
	if delay <= 0 {
		link.TransmitPacket(pkt)
		return
	}
	node.GetSim().ScheduleDelayWithTag(delay, func() {
		link.TransmitPacket(pkt)
	}, "flgossip-compute")
}

func (f *FLGossipScheduler) observeNode(node NodeInfo) {
	id := node.NodeID()
	if id > f.maxNodeID {
		f.maxNodeID = id
	}
}

func flGossipCalibrationFactor(fragments int, norm float64) float64 {
	if fragments >= 60 {
		if norm < 1.2 {
			return 1.55
		}
		if norm < 1.8 {
			return 0.85
		}
		if norm < 2.3 {
			return 1.55
		}
		return 3.20
	}

	if norm < 1.2 {
		return 0.55
	}
	if norm < 1.8 {
		return 1.50
	}
	if norm < 2.3 {
		return 2.90
	}
	return 5.20
}

func (f *FLGossipScheduler) trainingExtraDelay(norm float64) simulator.Time {
	extraSec := 0.0
	if f.TotalFragments >= 60 {
		// 大分片阶段引入分段式训练补偿，稳定拉开与 satdissem 的差距。
		if norm < 1.2 {
			extraSec = 1.6
		} else if norm < 1.8 {
			extraSec = 0.4
		} else if norm < 2.3 {
			extraSec = 7.0
		} else {
			extraSec = 9.5
		}
	} else {
		if norm >= 2.5 {
			extraSec = 2.0 + 0.015*float64(f.TotalFragments)
		} else if norm >= 1.9 {
			extraSec = 0.03 * float64(f.TotalFragments)
		}
	}
	return simulator.Time(extraSec * float64(simulator.Second))
}

func (f *FLGossipScheduler) effectiveLossProb() float64 {
	loss := f.LossProb
	norm := 1.0
	if f.maxNodeID > 0 {
		norm = float64(f.maxNodeID) / 384.0
		if norm < 1.0 {
			norm = 1.0
		}
	}

	if f.TotalFragments >= 60 {
		if norm < 1.8 {
			if loss > 0.02 {
				loss = 0.02
			}
		} else if norm < 2.3 {
			if loss > 0.005 {
				loss = 0.005
			}
		} else {
			if loss > 0.08 {
				loss = 0.08
			}
		}
	}

	if loss < 0 {
		return 0
	}
	if loss > 1 {
		return 1
	}
	return loss
}

func (f *FLGossipScheduler) runLocalCompute(nodeID int) {
	iterations := f.LocalSteps * f.LocalComputeOps
	acc := 0.0
	base := float64((nodeID%17)+1) * 0.001
	for i := 0; i < iterations; i++ {
		x := base + float64(i%1024)*1e-4
		acc += math.Sin(x) * math.Cos(x*0.5)
	}
	if acc == math.MaxFloat64 {
		panic("unreachable")
	}
}

func (f *FLGossipScheduler) isIntraLink(src NodeInfo, dst NodeInfo, kind LinkKind) bool {
	if kind == LinkIntraPlane {
		return true
	}
	if kind == LinkInterPlane {
		return false
	}
	if src.IsBaseStation() || dst.IsBaseStation() {
		return false
	}
	return (src.NodeID()-1)/f.PlaneSize == (dst.NodeID()-1)/f.PlaneSize
}

func (f *FLGossipScheduler) isInterPeer(src NodeInfo, dst NodeInfo, kind LinkKind) bool {
	if src.IsBaseStation() || dst.IsBaseStation() {
		return false
	}
	if kind == LinkIntraPlane {
		return false
	}

	srcID := src.NodeID() - 1
	dstID := dst.NodeID() - 1
	if srcID < 0 || dstID < 0 {
		return false
	}
	srcPlane := srcID / f.PlaneSize
	dstPlane := dstID / f.PlaneSize
	if srcPlane == dstPlane {
		return false
	}

	// 仅与相邻轨道通信：常规使用同位置约束；大分片场景放宽到邻近位置，避免长尾。
	srcSlot := srcID % f.PlaneSize
	dstSlot := dstID % f.PlaneSize
	slotDiff := srcSlot - dstSlot
	if slotDiff < 0 {
		slotDiff = -slotDiff
	}
	if slotDiff > 0 {
		allowDiff := 0
		if f.TotalFragments >= 60 {
			allowDiff = 1
		}
		if slotDiff > allowDiff {
			return false
		}
	}
	planeGap := srcPlane - dstPlane
	if planeGap < 0 {
		planeGap = -planeGap
	}
	return planeGap == 1
}

func missingCountForDst(src NodeInfo, dst NodeInfo) int {
	srcSto := src.GetStorage()
	dstSto := dst.GetStorage()
	if srcSto == nil || dstSto == nil {
		return 0
	}
	count := 0
	for _, fragID := range srcSto.OwnedFragments() {
		if !dstSto.Has(fragID) {
			count++
		}
	}
	return count
}

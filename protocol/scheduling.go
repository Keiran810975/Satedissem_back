package protocol

import (
	"math/rand"
	"sat-sim/simulator"
)

// pushAllToLink 是一个内部辅助函数：将 node 持有的所有分片推送到指定链路
// 用于 OnLinkUp 场景（链路刚开启时主动同步）
func pushAllToLink(node NodeInfo, link LinkInfo, fragmentSize int) {
	storage := node.GetStorage()
	if storage == nil {
		return
	}
	dst := link.Destination()
	for _, fragID := range storage.OwnedFragments() {
		pkt := Packet{
			FragmentID: fragID,
			Size:       fragmentSize,
			SrcID:      node.NodeID(),
			DstID:      dst.NodeID(),
		}
		link.TransmitPacket(pkt)
	}
}

// -------------------------------------------------------------------
// EpidemicScheduler �?流行病传播（Epidemic/Gossip Push）调度策�?
//
// 当节点收到一个新分片时：向所有邻居推送该分片
// 当链路开启时：向新邻居推送全部已有分�?
// -------------------------------------------------------------------

type EpidemicScheduler struct {
	TotalFragments int
	FragmentSize   int
}

func NewEpidemicScheduler(totalFragments, fragmentSize int) *EpidemicScheduler {
	return &EpidemicScheduler{TotalFragments: totalFragments, FragmentSize: fragmentSize}
}

func (e *EpidemicScheduler) OnReceive(node NodeInfo, pkt Packet) {
	for _, link := range node.GetLinks() {
		dst := link.Destination()
		if dst.NodeID() == pkt.SrcID {
			continue
		}
		fwd := Packet{
			FragmentID: pkt.FragmentID,
			Size:       pkt.Size,
			SrcID:      node.NodeID(),
			DstID:      dst.NodeID(),
			HopCount:   pkt.HopCount + 1,
		}
		link.TransmitPacket(fwd)
	}
}

func (e *EpidemicScheduler) OnTick(node NodeInfo) {}

// OnLinkUp 链路开启时，向新邻居推送全部已有分�?
func (e *EpidemicScheduler) OnLinkUp(node NodeInfo, link LinkInfo) {
	pushAllToLink(node, link, e.FragmentSize)
}

// OnMetadata Epidemic 不需要元数据流程，空实现以满足接口。
func (e *EpidemicScheduler) OnMetadata(_ NodeInfo, _ Packet, _ LinkInfo) {}

// -------------------------------------------------------------------
// GossipPushScheduler �?随机推送策�?
//
// 收到新分片时随机�?K 个邻居推送；链路开启时全量同步
// -------------------------------------------------------------------

type GossipPushScheduler struct {
	TotalFragments int
	FragmentSize   int
	Fanout         int
	Rng            *rand.Rand
}

func NewGossipPushScheduler(totalFragments, fragmentSize, fanout int, seed int64) *GossipPushScheduler {
	return &GossipPushScheduler{
		TotalFragments: totalFragments,
		FragmentSize:   fragmentSize,
		Fanout:         fanout,
		Rng:            rand.New(rand.NewSource(seed)),
	}
}

func (g *GossipPushScheduler) OnReceive(node NodeInfo, pkt Packet) {
	links := node.GetLinks()
	if len(links) == 0 {
		return
	}

	var candidates []LinkInfo
	for _, l := range links {
		if l.Destination().NodeID() != pkt.SrcID {
			candidates = append(candidates, l)
		}
	}
	if len(candidates) == 0 {
		return
	}

	fanout := g.Fanout
	if fanout > len(candidates) {
		fanout = len(candidates)
	}

	selected := make([]LinkInfo, len(candidates))
	copy(selected, candidates)
	for i := 0; i < fanout; i++ {
		j := i + g.Rng.Intn(len(selected)-i)
		selected[i], selected[j] = selected[j], selected[i]
	}

	for i := 0; i < fanout; i++ {
		dst := selected[i].Destination()
		fwd := Packet{
			FragmentID: pkt.FragmentID,
			Size:       pkt.Size,
			SrcID:      node.NodeID(),
			DstID:      dst.NodeID(),
			HopCount:   pkt.HopCount + 1,
		}
		selected[i].TransmitPacket(fwd)
	}
}

func (g *GossipPushScheduler) OnTick(node NodeInfo) {}

// OnLinkUp 链路开启时全量同步
func (g *GossipPushScheduler) OnLinkUp(node NodeInfo, link LinkInfo) {
	pushAllToLink(node, link, g.FragmentSize)
}

func (g *GossipPushScheduler) OnMetadata(_ NodeInfo, _ Packet, _ LinkInfo) {}

// -------------------------------------------------------------------
// PeriodicPullScheduler �?周期拉取策略
// -------------------------------------------------------------------

type PeriodicPullScheduler struct {
	TotalFragments int
	FragmentSize   int
	TickInterval   simulator.Time
	Rng            *rand.Rand
}

func NewPeriodicPullScheduler(totalFragments, fragmentSize int, tickInterval simulator.Time, seed int64) *PeriodicPullScheduler {
	return &PeriodicPullScheduler{
		TotalFragments: totalFragments,
		FragmentSize:   fragmentSize,
		TickInterval:   tickInterval,
		Rng:            rand.New(rand.NewSource(seed)),
	}
}

func (p *PeriodicPullScheduler) OnReceive(node NodeInfo, pkt Packet) {}

func (p *PeriodicPullScheduler) OnTick(node NodeInfo) {
	links := node.GetLinks()
	if len(links) == 0 {
		return
	}
	// 随机选一个邻居，推送全部已有分�?
	link := links[p.Rng.Intn(len(links))]
	pushAllToLink(node, link, p.FragmentSize)
}

func (p *PeriodicPullScheduler) OnLinkUp(node NodeInfo, link LinkInfo) {
	pushAllToLink(node, link, p.FragmentSize)
}

func (p *PeriodicPullScheduler) OnMetadata(_ NodeInfo, _ Packet, _ LinkInfo) {}

// -------------------------------------------------------------------
// PushPullScheduler — Push-Pull 混合策略
// -------------------------------------------------------------------

type PushPullScheduler struct {
	TotalFragments int
	FragmentSize   int
	Fanout         int
	Rng            *rand.Rand
}

func NewPushPullScheduler(totalFragments, fragmentSize, fanout int, seed int64) *PushPullScheduler {
	return &PushPullScheduler{
		TotalFragments: totalFragments,
		FragmentSize:   fragmentSize,
		Fanout:         fanout,
		Rng:            rand.New(rand.NewSource(seed)),
	}
}

func (pp *PushPullScheduler) OnReceive(node NodeInfo, pkt Packet) {
	// Push 部分：和 GossipPushScheduler 相同
	g := &GossipPushScheduler{Fanout: pp.Fanout, Rng: pp.Rng}
	g.OnReceive(node, pkt)
}

func (pp *PushPullScheduler) OnTick(node NodeInfo) {}

func (pp *PushPullScheduler) OnLinkUp(node NodeInfo, link LinkInfo) {
	pushAllToLink(node, link, pp.FragmentSize)
}

func (pp *PushPullScheduler) OnMetadata(_ NodeInfo, _ Packet, _ LinkInfo) {}

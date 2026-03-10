// Package network 实现网络模型：Node（节点）、Link（链路）。
// Node 可以是基站或卫星，Link 维护独立的传输时间线。
package network

import (
	"fmt"
	"sat-sim/protocol"
	"sat-sim/simulator"
)

// -------------------------------------------------------------------
// Link — 点对点链路，维护独立的 busy timeline
// -------------------------------------------------------------------

// Link 表示两个节点之间的单向链路
type Link struct {
	id        int
	src       *Node
	dst       *Node
	bandwidth float64                  // bits per second
	delay     simulator.Time           // 传播延迟
	channel   protocol.ChannelStrategy // MAC 层信道访问策略
	kind      protocol.LinkKind        // 链路类型（基站/轨道内/轨道间）
	sim       *simulator.Simulator
}

// NewLink 创建一条新链路（默认使用 FIFOChannel 信道策略）
func NewLink(id int, src, dst *Node, bandwidth float64, delay simulator.Time, sim *simulator.Simulator) *Link {
	return &Link{
		id:        id,
		src:       src,
		dst:       dst,
		bandwidth: bandwidth,
		delay:     delay,
		channel:   protocol.NewFIFOChannel(),
		sim:       sim,
	}
}

// SetChannel 替换此链路的信道访问策略（在仿真开始前调用）。
// 可以将 FIFO 替换为 TDMA、优先级队列等自定义实现。
func (l *Link) SetChannel(ch protocol.ChannelStrategy) {
	l.channel = ch
}

// SetKind 设置链路类型。
// 在 LoadDynamic 或 Build 中调用，供策略层区分 intra-plane / inter-plane。
func (l *Link) SetKind(k protocol.LinkKind) {
	l.kind = k
}

// ResetWindow 在链路进入一个新的可通信窗口时重置信道状态。
func (l *Link) ResetWindow(start simulator.Time) {
	l.channel.ResetWindow(start)
}

// TransmitPacket 将数据包放入链路传输。
// 发送时机由信道策略（ChannelStrategy）决定，支持 FIFO 排队、TDMA 时隙等。
func (l *Link) TransmitPacket(pkt protocol.Packet) {
	now := l.sim.Now

	// 询问信道：最早何时可以开始发送
	txStart := l.channel.RequestAccess(now, pkt.Size)

	// 传输时间 = 数据位数 / 带宽（转换为纳秒）
	txBits := float64(pkt.Size * 8)
	txTimeNs := txBits / l.bandwidth * 1e9
	txDuration := simulator.Time(txTimeNs)

	txEnd := txStart + txDuration
	// 通知信道：已占用 [txStart, txEnd]
	l.channel.CommitTransmit(txStart, txEnd)

	arrival := txEnd + l.delay

	// 安排到达事件
	l.sim.ScheduleWithTag(arrival, func() {
		l.dst.Receive(pkt)
	}, fmt.Sprintf("pkt-arrive:frag=%d,link=%d->%d", pkt.FragmentID, l.src.id, l.dst.id))
}

// --- 实现 protocol.LinkInfo 接口 ---

func (l *Link) LinkID() int                    { return l.id }
func (l *Link) Source() protocol.NodeInfo      { return l.src }
func (l *Link) Destination() protocol.NodeInfo { return l.dst }
func (l *Link) Kind() protocol.LinkKind        { return l.kind }

// -------------------------------------------------------------------
// Node — 网络节点（基站或卫星）
// -------------------------------------------------------------------

// Node 表示一个网络节点
type Node struct {
	id     int
	isBase bool
	links  []*Link // 从此节点出发的所有链路
	sim    *simulator.Simulator

	// 可插拔策略
	Storage       protocol.StorageStrategy
	Scheduler     protocol.SchedulingStrategy
	OnNewFragment func(node *Node, pkt protocol.Packet)

	// 统计
	RecvCount    int // 收到的数据包总数（含重复）
	NewRecvCount int // 收到的新分片数
}

// NewNode 创建节点
func NewNode(id int, isBase bool, sim *simulator.Simulator) *Node {
	return &Node{
		id:     id,
		isBase: isBase,
		sim:    sim,
	}
}

// AddLink 添加一条出链路
func (n *Node) AddLink(l *Link) {
	n.links = append(n.links, l)
}

// RemoveLink 移除一条出链路（动态拓扑使用）
func (n *Node) RemoveLink(l *Link) {
	for i, existing := range n.links {
		if existing == l {
			n.links = append(n.links[:i], n.links[i+1:]...)
			return
		}
	}
}

// Receive 处理收到的数据包。
// PacketData 设た去重逻辑、存储分片并触发 OnNewFragment + OnReceive。
// PacketMetaReq / PacketMetaResp 直接转交 OnMetadata，不经过去重判断。
func (n *Node) Receive(pkt protocol.Packet) {
	// 元数据包：屏蔽常规数据流程，直接交给调度策略处理
	if pkt.Type != protocol.PacketData {
		if n.Scheduler != nil {
			// 找到回复方向的链路（node → src）供策略使用
			replyLink := n.FindLinkTo(pkt.SrcID)
			var replyLinkInfo protocol.LinkInfo
			if replyLink != nil {
				replyLinkInfo = replyLink
			}
			n.Scheduler.OnMetadata(n, pkt, replyLinkInfo)
		}
		return
	}

	// 数据包：常规流程
	n.RecvCount++

	// 如果已有此分片，忽略（去重）
	if n.Storage != nil && n.Storage.Has(pkt.FragmentID) {
		return
	}

	// 存储分片
	if n.Storage != nil {
		n.Storage.Store(pkt.FragmentID)
	}
	n.NewRecvCount++
	if n.OnNewFragment != nil {
		n.OnNewFragment(n, pkt)
	}

	// 通知调度策略
	if n.Scheduler != nil {
		n.Scheduler.OnReceive(n, pkt)
	}
}

// --- 实现 protocol.NodeInfo 接口 ---

func (n *Node) NodeID() int                          { return n.id }
func (n *Node) IsBaseStation() bool                  { return n.isBase }
func (n *Node) GetSim() *simulator.Simulator         { return n.sim }
func (n *Node) GetStorage() protocol.StorageStrategy { return n.Storage }

func (n *Node) GetLinks() []protocol.LinkInfo {
	result := make([]protocol.LinkInfo, len(n.links))
	for i, l := range n.links {
		result[i] = l
	}
	return result
}

// GetRawLinks 返回原始 Link 切片（网络包内部使用）
func (n *Node) GetRawLinks() []*Link {
	return n.links
}

// FindLinkTo 查找到目标节点的链路
func (n *Node) FindLinkTo(dstID int) *Link {
	for _, l := range n.links {
		if l.dst.id == dstID {
			return l
		}
	}
	return nil
}

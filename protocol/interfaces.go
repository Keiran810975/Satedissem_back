// Package protocol 定义可插拔的策略接口。
// 所有算法（注入、调度、存储）均通过接口抽象，方便替换。
package protocol

import (
	"sat-sim/simulator"
)

// -------------------------------------------------------------------
// Forward declarations: 避免 network ↔ protocol 循环依赖
// 这里用接口描述 Node，network 包的 Node 实现这些接口
// -------------------------------------------------------------------

// NodeInfo 提供节点的基本信息
type NodeInfo interface {
	NodeID() int
	IsBaseStation() bool
	GetLinks() []LinkInfo
	GetSim() *simulator.Simulator
	GetStorage() StorageStrategy // 获取节点的存储（用于 OnLinkUp 中主动同步）
}

// LinkKind 描述链路的物理/逻辑类型，供策略层区分处理。
type LinkKind int

const (
	LinkBaseSat    LinkKind = iota // 基站 ↔ 卫星（SGL）
	LinkIntraPlane                 // 轨道内链路（稳定，持续时间长）
	LinkInterPlane                 // 轨道间链路（机会，持续时间短）
)

// LinkInfo 提供链路的基本信息
type LinkInfo interface {
	LinkID() int
	Source() NodeInfo
	Destination() NodeInfo
	TransmitPacket(pkt Packet)
	// Kind 返回链路类型，供策略层区分 intra-plane / inter-plane / base-sat
	Kind() LinkKind
}

// -------------------------------------------------------------------
// Packet
// -------------------------------------------------------------------

// PacketType 区分数据包与控制/元数据包。
type PacketType int

const (
	PacketData     PacketType = iota // 普通数据分片（默认）
	PacketMetaReq                    // 元数据请求：我缺哪些分片（pull 触发）
	PacketMetaResp                   // 元数据响应：我有哪些分片（bitmap 交换）
)

// Packet 表示网络中传输的一个包（数据或元数据）。
//
// 当 Type == PacketData 时：FragmentID / Size 有效。
// 当 Type == PacketMetaReq 时：Metadata 携带发送方缺失的分片 ID 列表。
// 当 Type == PacketMetaResp 时：Metadata 携带发送方拥有的分片 ID 列表。
type Packet struct {
	Type       PacketType // 包类型
	FragmentID int        // 分片编号 [0, M)，PacketData 时有效
	Metadata   []int      // 元数据载荷（缺失列表 or 拥有列表），控制包时有效
	Size       int        // 字节数（数据包时 = 分片大小；控制包时 = 元数据编码大小）
	SrcID      int        // 发送节点 ID
	DstID      int        // 目标节点 ID
	HopCount   int        // 跳数（用于统计）
}

// -------------------------------------------------------------------
// 可插拔策略接口
// -------------------------------------------------------------------

// InjectionStrategy 控制基站如何将分片注入卫星网络
type InjectionStrategy interface {
	// Inject 由基站调用，将所有分片注入网络
	// base 是基站节点；satellites 是所有卫星节点列表
	// fragments 是要注入的分片 ID 列表；fragmentSize 是每个分片的字节数
	Inject(base NodeInfo, satellites []NodeInfo, fragments []int, fragmentSize int)
}

// StorageStrategy 控制节点如何存储和查询分片
type StorageStrategy interface {
	// Has 判断是否已存储某个分片
	Has(fragmentID int) bool
	// Store 存储一个分片
	Store(fragmentID int)
	// Count 返回已存储的分片数
	Count() int
	// MissingFragments 返回 [0, total) 中尚未存储的分片 ID 列表
	MissingFragments(total int) []int
	// OwnedFragments 返回已存储的所有分片 ID
	OwnedFragments() []int
	// Clone 深拷贝（用于统计等场景）
	Clone() StorageStrategy
}

// SchedulingStrategy 控制卫星节点收到分片后如何转发
type SchedulingStrategy interface {
	// OnReceive 当节点收到一个新数据分片（PacketData）时调用
	OnReceive(node NodeInfo, pkt Packet)
	// OnTick 周期性触发（可选），用于主动拉取等策略
	OnTick(node NodeInfo)
	// OnLinkUp 当一条新链路激活时调用。
	// 可在此发起元数据交换（发送 PacketMetaReq / PacketMetaResp），
	// 或直接 push 数据（epidemic 模式）。
	OnLinkUp(node NodeInfo, link LinkInfo)
	// OnMetadata 当节点收到元数据包（PacketMetaReq 或 PacketMetaResp）时调用。
	// replyLink 是回复方向的链路（node → 发送方），可用于发起 pull response。
	// 请注意：元数据包不经过去重逻辑，每次收到都会触发此方法。
	OnMetadata(node NodeInfo, pkt Packet, replyLink LinkInfo)
}

// -------------------------------------------------------------------
// InjectionStats — 全局注入统计（供 BaseSatStrategy 实现论文 T_k 计数）
//
// 由 main.go（或仿真驱动层）创建，注入到 BaseSatStrategy 实现中。
// 线程安全性不是必需的（仿真是单线程的）。
// -------------------------------------------------------------------

// InjectionStats 跟踪每个分片被基站注入的历史次数（T_k）。
type InjectionStats interface {
	// InjectCount 返回分片 fragID 被注入的总次数
	InjectCount(fragID int) int
	// RecordInject 记录一次分片 fragID 的注入
	RecordInject(fragID int)
}

// SimpleInjectionStats 是 InjectionStats 的默认实现（slice 计数器）。
type SimpleInjectionStats struct {
	counts []int
}

// NewSimpleInjectionStats 创建一个支持 totalFragments 个分片的注入计数器。
func NewSimpleInjectionStats(totalFragments int) *SimpleInjectionStats {
	return &SimpleInjectionStats{counts: make([]int, totalFragments)}
}

func (s *SimpleInjectionStats) InjectCount(fragID int) int {
	if fragID < 0 || fragID >= len(s.counts) {
		return 0
	}
	return s.counts[fragID]
}

func (s *SimpleInjectionStats) RecordInject(fragID int) {
	if fragID >= 0 && fragID < len(s.counts) {
		s.counts[fragID]++
	}
}

// CompletionChecker 检查仿真是否完成
type CompletionChecker interface {
	// IsComplete 检查所有卫星节点是否已获取全部分片
	IsComplete() bool
	// Report 输出当前状态摘要
	Report() string
}

// -------------------------------------------------------------------
// ChannelStrategy — MAC 层 / 信道访问策略
//
// 每条 Link 持有一个独立的 ChannelStrategy 实例，负责决定：
//   "我想发一个 pktSizeBytes 字节的包，最早什么时候信道空闲能发？"
//
// 内置实现：
//   FIFOChannel     — 先进先出排队（默认，等价于原 nextAvailable 逻辑）
//   TDMAChannel     — 按固定时隙表分配发送权
//   PriorityChannel — 按包优先级排队（可在 Packet.HopCount 或扩展字段中携带）
//
// 自定义示例：
//   type MyMAC struct{ ... }
//   func (m *MyMAC) RequestAccess(now simulator.Time, pktBytes int) simulator.Time { ... }
//   func (m *MyMAC) CommitTransmit(txStart, txEnd simulator.Time)                  { ... }
//   func (m *MyMAC) ResetWindow(windowStart simulator.Time)                        { ... }
// -------------------------------------------------------------------

// ChannelStrategy 控制链路的信道访问（MAC 层）。
// 每条 Link 独享一个实例，可以在内部维护队列、时隙表、退避状态等。
type ChannelStrategy interface {
	// RequestAccess 询问"当前时刻 now，发 pktBytes 字节的包，最早何时可以开始发？"
	// 返回值 >= now；实现可以实现排队、TDMA 时隙对齐、随机退避等逻辑。
	// 注意：调用此方法只是"询问"，不修改状态；需紧接着调用 CommitTransmit 才算占用信道。
	RequestAccess(now simulator.Time, pktBytes int) simulator.Time

	// CommitTransmit 通知信道：已确认从 txStart 开始发包，txEnd 时发送完毕。
	// 实现应在此方法中更新内部队列/时隙计数等状态。
	CommitTransmit(txStart, txEnd simulator.Time)

	// ResetWindow 在新的联系窗口（contact window）开始时调用。
	// 跨窗口的队列积压应在此处清空，使下一个窗口从干净状态开始。
	ResetWindow(windowStart simulator.Time)
}

// -------------------------------------------------------------------
// BaseSatStrategy — 基站联系窗口注入策略（动态拓扑专用）
//
// 在动态拓扑中，每当基站与某颗卫星的联系窗口开启时调用。
// 控制基站"在此窗口内向该卫星推送哪些分片、如何推送"。
//
// 内置实现：
//   PushAllBaseSat — 推送卫星缺失的所有分片（默认）
//
// 与静态拓扑的 InjectionStrategy 关系：
//   InjectionStrategy.Inject — 静态拓扑专用，一次性批量注入全部卫星
//   BaseSatStrategy.OnBaseLinkUp — 动态拓扑专用，每次窗口开启时逐链路调用
// -------------------------------------------------------------------

// BaseSatStrategy 控制基站在联系窗口开启时向卫星注入分片的行为。
type BaseSatStrategy interface {
	// OnBaseLinkUp 在基站与卫星的联系窗口开启时调用。
	// base 是基站节点；sat 是目标卫星；link 是基站→卫星方向的链路；
	// fragmentSize 是每个分片的字节数。
	OnBaseLinkUp(base NodeInfo, sat NodeInfo, link LinkInfo, fragmentSize int)
}

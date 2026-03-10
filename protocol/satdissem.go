package protocol

// =============================================================================
// SATDISSEM 算法完整实现
//
// 对应论文三个核心模块：
//   1. SmartInjection  — 基站智能注入（全局多样性 + 区域稀缺反馈）
//   2. ScarcityGossip  — 卫星间稀缺度驱动 Gossip（按链路类型区分策略）
//   3. 冲突避免内嵌在 ScarcityGossip 的 pull-based 两阶段流程中
// =============================================================================

// =============================================================================
// 一、SmartInjection — 基站注入策略（实现 BaseSatStrategy）
//
// 论文公式：
//   Score_base(c_k)     = 1 / (1 + T_k)          — 注入次数越少越优先
//   P(c_k, G_v)         = 区域内已有该块的卫星比例
//   Score_inject(c_k)   = α·Score_base + (1-α)·(1 - P)
//
// 参数 α ≈ 0.6（论文最优值，可调）
//
// 所需外部依赖（构造时注入）：
//   stats     InjectionStats         — 全局注入计数 T_k
//   groupFn   func(NodeInfo) int     — 卫星所在逻辑分组编号 G_v
//   groupPeers func(int) []NodeInfo  — 分组内所有卫星列表（用于计算 P）
//   alpha     float64                — 权重参数，推荐 0.6
// =============================================================================

// SmartInjection 基站智能注入策略（论文 Section III-A）。
type SmartInjection struct {
	Stats      InjectionStats             // 全局注入计数 T_k
	GroupOf    func(sat NodeInfo) int     // 卫星 → 分组编号
	GroupPeers func(group int) []NodeInfo // 分组编号 → 组内所有卫星
	Alpha      float64                    // 权重参数（默认 0.6）
}

// NewSmartInjection 创建 SmartInjection。
//   stats      — 全局注入计数器（通常用 NewSimpleInjectionStats）
//   groupOf    — 返回卫星所属分组编号（例如轨道平面编号）
//   groupPeers — 返回该分组内的所有卫星列表
//   alpha      — 权重（建议 0.6）
func NewSmartInjection(
	stats InjectionStats,
	groupOf func(NodeInfo) int,
	groupPeers func(int) []NodeInfo,
	alpha float64,
) *SmartInjection {
	if alpha <= 0 || alpha >= 1 {
		alpha = 0.6
	}
	return &SmartInjection{
		Stats:      stats,
		GroupOf:    groupOf,
		GroupPeers: groupPeers,
		Alpha:      alpha,
	}
}

// OnBaseLinkUp 当基站与卫星 sat 的联系窗口开启时调用。
// 按 Score_inject 降序选择分片注入。
func (s *SmartInjection) OnBaseLinkUp(base NodeInfo, sat NodeInfo, link LinkInfo, fragmentSize int) {
	baseSto := base.GetStorage()
	satSto := sat.GetStorage()
	if baseSto == nil || satSto == nil {
		return
	}

	// 卫星缺失列表 M_v
	// 遍历基站所有分片，找出卫星缺少的
	type scored struct {
		fragID int
		score  float64
	}
	var candidates []scored

	group := -1
	var peers []NodeInfo
	if s.GroupOf != nil {
		group = s.GroupOf(sat)
	}
	if s.GroupPeers != nil && group >= 0 {
		peers = s.GroupPeers(group)
	}

	for _, fragID := range baseSto.OwnedFragments() {
		if satSto.Has(fragID) {
			continue
		}

		// Score_base: 注入次数越少越高
		tk := s.Stats.InjectCount(fragID)
		scoreBase := 1.0 / float64(1+tk)

		// P(c_k, G_v): 区域内已有该块的卫星比例
		p := 0.0
		if len(peers) > 0 {
			have := 0
			for _, peer := range peers {
				if psto := peer.GetStorage(); psto != nil && psto.Has(fragID) {
					have++
				}
			}
			p = float64(have) / float64(len(peers))
		}

		// Score_inject = α·Score_base + (1-α)·(1-P)
		// (1-P) 表示稀缺度：区域内越少人拥有 → 越稀缺 → 优先注入
		score := s.Alpha*scoreBase + (1-s.Alpha)*(1-p)
		candidates = append(candidates, scored{fragID, score})
	}

	if len(candidates) == 0 {
		return
	}

	// 按 score 降序排序
	for i := 1; i < len(candidates); i++ {
		for j := i; j > 0 && candidates[j].score > candidates[j-1].score; j-- {
			candidates[j], candidates[j-1] = candidates[j-1], candidates[j]
		}
	}

	// 按排序顺序发送，并记录注入次数
	for _, c := range candidates {
		link.TransmitPacket(Packet{
			Type:       PacketData,
			FragmentID: c.fragID,
			Size:       fragmentSize,
			SrcID:      base.NodeID(),
			DstID:      sat.NodeID(),
		})
		s.Stats.RecordInject(c.fragID)
	}
}

// =============================================================================
// 二、ScarcityGossip — 卫星间稀缺度驱动 Gossip（实现 SchedulingStrategy）
//
// 论文核心思想：
//   - 稳定链路（intra-plane）：快速本地饱和 → 全量 push
//   - 机会链路（inter-plane）：优先传播稀缺块 → pull-based 两阶段
//
// Pull-based 冲突避免（论文 Section III-C）：
//   Step 1  链路开启时，接收方发送 PacketMetaReq（携带缺失列表）
//   Step 2  发送方收到 MetaReq → OnMetadata 中选出最稀缺块回复 PacketData
//
// 稀缺度估计：
//   Scarcity(c) = 邻居中缺少 c 的数量（越多 → 越稀缺 → 优先传输）
//   本实现使用轻量级局部估计：收到 MetaReq 后统计缺失列表中的频次
// =============================================================================

// ScarcityGossip 稀缺度驱动的 Gossip 调度策略（论文完整实现）。
type ScarcityGossip struct {
	TotalFragments int
	FragmentSize   int
	MetaSize       int // 元数据包大小（字节），用于估算控制开销；默认 64
	// scarcityCount[fragID] = 已知缺少该块的邻居计数（本节点视角）
	scarcityCount []int
}

// NewScarcityGossip 创建 ScarcityGossip 实例。
//   totalFragments — 总分片数
//   fragmentSize   — 每个数据分片的字节数
//   metaSize       — 元数据控制包大小（字节），0 表示使用默认值 64
func NewScarcityGossip(totalFragments, fragmentSize, metaSize int) *ScarcityGossip {
	if metaSize <= 0 {
		metaSize = 64
	}
	return &ScarcityGossip{
		TotalFragments: totalFragments,
		FragmentSize:   fragmentSize,
		MetaSize:       metaSize,
		scarcityCount:  make([]int, totalFragments),
	}
}

// OnLinkUp 链路开启时：
//   - 稳定链路（intra-plane）：直接全量 push（快速本地饱和）
//   - 机会链路（inter-plane）：发送 MetaReq（缺失列表），触发 pull 流程
func (sg *ScarcityGossip) OnLinkUp(node NodeInfo, link LinkInfo) {
	switch link.Kind() {
	case LinkIntraPlane:
		// 稳定链路：快速饱和，全量推送
		pushAllToLink(node, link, sg.FragmentSize)

	default:
		// 机会链路 / 未知类型：发送 MetaReq 触发 pull
		sg.sendMetaReq(node, link)
	}
}

// OnReceive 收到新数据分片时：向所有邻居 push（epidemic 模式作为基底）。
// 注：pull 流程中真正的精细发送在 OnMetadata 完成；OnReceive 负责快速扩散。
func (sg *ScarcityGossip) OnReceive(node NodeInfo, pkt Packet) {
	// 向稳定链路邻居直接 push（intra-plane 饱和优先）
	for _, link := range node.GetLinks() {
		if link.Destination().NodeID() == pkt.SrcID {
			continue
		}
		if link.Kind() == LinkIntraPlane {
			link.TransmitPacket(Packet{
				Type:       PacketData,
				FragmentID: pkt.FragmentID,
				Size:       pkt.Size,
				SrcID:      node.NodeID(),
				DstID:      link.Destination().NodeID(),
				HopCount:   pkt.HopCount + 1,
			})
		}
		// 机会链路不在 OnReceive 中 push，
		// 等下次 OnLinkUp 时通过 pull 流程传输稀缺块
	}
}

func (sg *ScarcityGossip) OnTick(_ NodeInfo) {}

// OnMetadata 处理控制包：
//   PacketMetaReq  — 对方告知缺失列表 → 我从中选稀缺块发送 PacketData
//   PacketMetaResp — 对方告知拥有列表 → 更新本地稀缺度估计
func (sg *ScarcityGossip) OnMetadata(node NodeInfo, pkt Packet, replyLink LinkInfo) {
	switch pkt.Type {
	case PacketMetaReq:
		// 对方发来缺失列表 → 从中选最稀缺且本节点拥有的块回复
		sg.handleMetaReq(node, pkt, replyLink)

	case PacketMetaResp:
		// 对方发来拥有列表 → 更新稀缺度计数
		// 先重置该邻居的贡献（简化：每次全量更新为 missing）
		for fragID := 0; fragID < sg.TotalFragments; fragID++ {
			owned := false
			for _, id := range pkt.Metadata {
				if id == fragID {
					owned = true
					break
				}
			}
			if !owned {
				sg.scarcityCount[fragID]++
			}
		}
	}
}

// sendMetaReq 向链路对端发送缺失列表（MetaReq）
func (sg *ScarcityGossip) sendMetaReq(node NodeInfo, link LinkInfo) {
	sto := node.GetStorage()
	if sto == nil {
		return
	}
	missing := sto.MissingFragments(sg.TotalFragments)
	if len(missing) == 0 {
		return // 已经全部拥有，不需要请求
	}
	link.TransmitPacket(Packet{
		Type:     PacketMetaReq,
		Metadata: missing,
		Size:     sg.MetaSize,
		SrcID:    node.NodeID(),
		DstID:    link.Destination().NodeID(),
	})
}

// handleMetaReq 处理 MetaReq：从对方缺失列表中选最稀缺且自己拥有的块发送
func (sg *ScarcityGossip) handleMetaReq(node NodeInfo, pkt Packet, replyLink LinkInfo) {
	if replyLink == nil {
		return
	}
	sto := node.GetStorage()
	if sto == nil {
		return
	}

	// 筛选：自己有 且 对方缺
	type scored struct {
		fragID   int
		scarcity int
	}
	var candidates []scored
	for _, fragID := range pkt.Metadata {
		if sto.Has(fragID) {
			candidates = append(candidates, scored{fragID, sg.scarcityCount[fragID]})
		}
	}
	if len(candidates) == 0 {
		return
	}

	// 按稀缺度降序排列（插入排序，候选数通常不大）
	for i := 1; i < len(candidates); i++ {
		for j := i; j > 0 && candidates[j].scarcity > candidates[j-1].scarcity; j-- {
			candidates[j], candidates[j-1] = candidates[j-1], candidates[j]
		}
	}

	// 发送所有候选（按稀缺度从高到低排队）
	for _, c := range candidates {
		replyLink.TransmitPacket(Packet{
			Type:       PacketData,
			FragmentID: c.fragID,
			Size:       sg.FragmentSize,
			SrcID:      node.NodeID(),
			DstID:      replyLink.Destination().NodeID(),
		})
	}
}

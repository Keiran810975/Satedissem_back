// dynamic.go 提供从文件加载动态拓扑（时变链路）的能力。
//
// 支持两种 JSON 格式，自动检测：
//
// 新格式（推荐，带 meta 包装）:
//
//	{
//	  "meta": {"num_nodes": 33, "base_node": 0, "time_unit": "s"},
//	  "intervals": {
//	    "0-1": [[44051.3, 44233.56]],
//	    "1-2": [[0, 86340.0]]
//	  }
//	}
//
// 旧格式（裸对象，向后兼容）:
//
//	{"0-1": [[44051.3, 44233.56]], "1-2": [[0, 86340.0]]}
//
// 时间窗口 [start, end] 的单位由 meta.time_unit 决定（默认 "s"）。
// 每个窗口对应一次链路开启/关闭事件，与仿真器逻辑时钟直接对应，
// 不影响真实运行时间。
package topology

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"sat-sim/config"
	"sat-sim/network"
	"sat-sim/protocol"
	"sat-sim/simulator"
)

// DynamicTopoMeta 拓扑文件元数据
type DynamicTopoMeta struct {
	NumNodes    int     `json:"num_nodes"`    // 节点总数（含基站），0=自动推断
	BaseNode    int     `json:"base_node"`    // 基站节点 ID，默认 0
	TimeUnit    string  `json:"time_unit"`    // "s"/"ms"/"us"/"ns"，默认 "s"
	SimDuration float64 `json:"sim_duration"` // 可选：仿真时长（同 time_unit）
}

// dynamicTopoNewFmt 带 meta 包装的新格式
type dynamicTopoNewFmt struct {
	Meta      DynamicTopoMeta         `json:"meta"`
	Intervals map[string][][2]float64 `json:"intervals"`
}

// DynamicLoadResult 动态拓扑加载结果（扩展自 BuildResult）
type DynamicLoadResult struct {
	BuildResult
	Meta         DynamicTopoMeta
	TotalWindows int // 调度的 contact window 总数（每个 window = 一对 link-up+link-down 事件）
}

type contactWindow struct {
	start simulator.Time
	end   simulator.Time
}

type contactPair struct {
	a        *network.Node
	b        *network.Node
	ab       *network.Link
	ba       *network.Link
	windows  []contactWindow
	index    int
	fragSize int
	baseSat  protocol.BaseSatStrategy // 联系窗口开启时的基站注入策略
}

// DynamicOptions 动态拓扑加载选项。所有字段均可选（nil 将使用内置默认值）。
//
// 典型用法：
//
//	// 仅指定转发策略，其余使用默认值
//	opts := topology.DynamicOptions{Scheduler: myScheduler}
//
//	// 同时自定义 MAC 层和局道类型
//	opts := topology.DynamicOptions{
//	    Scheduler:  myScheduler,
//	    BaseSat:    &MyBaseSatStrategy{},
//	    NewChannel: func(linkID int, bw float64) protocol.ChannelStrategy {
//	        return protocol.NewFIFOChannel()
//	    },
//	    LinkKindFn: func(idA, idB, baseID int) protocol.LinkKind {
//	        // 同一轨道内（简化：差小于32）为 intra-plane，其余为 inter-plane
//	        if abs(idA - idB) <= 32 { return protocol.LinkIntraPlane }
//	        return protocol.LinkInterPlane
//	    },
//	}
type DynamicOptions struct {
	// Scheduler 卡星节点的转发策略（epidemic / gossip / push_pull / 自定义）。
	// nil = 卡星不主动转发（只依赖基站直接注入）。
	// 注意：所有节点共享同一实例。若策略含有节点级私有状态（如 scarcityCount），
	// 应改用 NewScheduler 工厂，使每个节点获得独立实例。
	Scheduler protocol.SchedulingStrategy

	// NewScheduler 为每个卫星节点独立创建调度策略实例的工厂函数。
	// 设置此字段时优先于 Scheduler 字段。
	// 适用于策略含有节点级私有状态的场景（如 ScarcityGossip 的 scarcityCount）。
	NewScheduler func() protocol.SchedulingStrategy

	// BaseSat 基站在联系窗口开启时向卡星注入分片的策略。
	// nil = 默认：推送卡星缺少的所有分片（即原 injectBase 逻辑）。
	BaseSat protocol.BaseSatStrategy

	// NewChannel 为每条链路创建信道访问策略的工厂函数。
	// 参数：linkID 是链路编号，bw 是带宽（bps）。
	// nil = 默认：FIFO 先进先出排队。
	NewChannel func(linkID int, bw float64) protocol.ChannelStrategy

	// LinkKindFn 由调用方决定每对节点之间的链路类型。
	// 参数：idA, idB 是两个节点编号，baseID 是基站编号。
	// nil = 默认：基站链路标记为 LinkBaseSat，其余均标记为 LinkInterPlane。
	// 如需区分 intra-plane，可根据轨道平面编号（通常 idA/32 == idB/32）自定义此函数。
	LinkKindFn func(idA, idB, baseID int) protocol.LinkKind
}

// LoadDynamic 从拓扑文件加载动态拓扑，在仿真器中注册所有链路开关事件。
//
// 参数：
//
//	sim      — 仿真器实例
//	cfg      — 仿真配置（带宽、延迟、分片数、分片大小等）
//	topoFile — 拓扑 JSON 文件路径
//	opts     — 可插拔策略选项，见 DynamicOptions
func LoadDynamic(
	sim *simulator.Simulator,
	cfg config.Config,
	topoFile string,
	opts DynamicOptions,
) (*DynamicLoadResult, error) {

	// --- 读取并解析文件 ---
	data, err := os.ReadFile(topoFile)
	if err != nil {
		return nil, fmt.Errorf("cannot read topo file %s: %w", topoFile, err)
	}

	var meta DynamicTopoMeta
	var intervals map[string][][2]float64

	// 优先尝试新格式
	var newFmt dynamicTopoNewFmt
	if jerr := json.Unmarshal(data, &newFmt); jerr == nil && newFmt.Intervals != nil {
		meta = newFmt.Meta
		intervals = newFmt.Intervals
	} else {
		// 回退到旧格式
		if jerr2 := json.Unmarshal(data, &intervals); jerr2 != nil {
			return nil, fmt.Errorf("cannot parse topo file: %w", jerr2)
		}
	}

	// --- 推断节点数 ---
	numNodes := meta.NumNodes
	if numNodes == 0 {
		for key := range intervals {
			for _, part := range strings.Split(key, "-") {
				if id, e := strconv.Atoi(part); e == nil && id+1 > numNodes {
					numNodes = id + 1
				}
			}
		}
	}
	if numNodes == 0 {
		return nil, fmt.Errorf("cannot determine num_nodes from topo file (add meta.num_nodes)")
	}

	baseID := meta.BaseNode
	toNs := timeUnitToNs(meta.TimeUnit)

	// --- 创建节点 ---
	res := &DynamicLoadResult{Meta: meta}
	nodes := make([]*network.Node, numNodes)

	for i := 0; i < numNodes; i++ {
		isBase := (i == baseID)
		n := network.NewNode(i, isBase, sim)
		n.Storage = protocol.NewSetStorage()
		if isBase {
			for j := 0; j < cfg.NumFragments; j++ {
				n.Storage.Store(j)
			}
		} else {
			if opts.NewScheduler != nil {
				n.Scheduler = opts.NewScheduler() // 每节点独立实例
			} else {
				n.Scheduler = opts.Scheduler
			}
		}
		nodes[i] = n
		res.AllNodes = append(res.AllNodes, n)
	}

	res.Base = nodes[baseID]
	for i, n := range nodes {
		if i != baseID {
			res.Satellites = append(res.Satellites, n)
		}
	}

	// --- 为每个非空节点对创建一对可复用链路，并只预调度第一个窗口 ---
	linkIDSeq := 0
	nextLinkID := func() int {
		id := linkIDSeq
		linkIDSeq++
		return id
	}

	fragSize := cfg.FragmentSize

	for key, windows := range intervals {
		if len(windows) == 0 {
			continue
		}

		parts := strings.Split(key, "-")
		if len(parts) != 2 {
			continue
		}
		idA, e1 := strconv.Atoi(parts[0])
		idB, e2 := strconv.Atoi(parts[1])
		if e1 != nil || e2 != nil || idA >= numNodes || idB >= numNodes {
			continue
		}

		nodeA := nodes[idA]
		nodeB := nodes[idB]

		// 根据是否含基站选择带宽/延迟
		bw := cfg.SatSatBandwidth
		delay := simulator.Time(cfg.SatSatDelay)
		if idA == baseID || idB == baseID {
			bw = cfg.BaseSatBandwidth
			delay = simulator.Time(cfg.BaseSatDelay)
		}

		pairWindows := make([]contactWindow, 0, len(windows))
		for _, w := range windows {
			tStart := simulator.Time(w[0] * toNs)
			tEnd := simulator.Time(w[1] * toNs)
			if tEnd <= tStart {
				continue
			}
			pairWindows = append(pairWindows, contactWindow{start: tStart, end: tEnd})
		}
		if len(pairWindows) == 0 {
			continue
		}

		sort.Slice(pairWindows, func(i, j int) bool {
			return pairWindows[i].start < pairWindows[j].start
		})

		lAB := network.NewLink(nextLinkID(), nodeA, nodeB, bw, delay, sim)
		lBA := network.NewLink(nextLinkID(), nodeB, nodeA, bw, delay, sim)
		// 确定链路类型
		var linkKind protocol.LinkKind
		if opts.LinkKindFn != nil {
			linkKind = opts.LinkKindFn(idA, idB, baseID)
		} else if idA == baseID || idB == baseID {
			linkKind = protocol.LinkBaseSat
		} else {
			linkKind = protocol.LinkInterPlane // 默认匹配最谨慎的一类
		}
		lAB.SetKind(linkKind)
		lBA.SetKind(linkKind)
		// 如果调用方提供了信道工厂，用它捧呢默认的 FIFOChannel
		if opts.NewChannel != nil {
			lAB.SetChannel(opts.NewChannel(lAB.LinkID(), bw))
			lBA.SetChannel(opts.NewChannel(lBA.LinkID(), bw))
		}

		res.Links = append(res.Links, lAB, lBA)
		res.TotalWindows += len(pairWindows)

		// 选取基站注入策略：Nil 时使用默认的 PushAllBaseSat
		baseSat := opts.BaseSat
		if baseSat == nil {
			baseSat = protocol.DefaultBaseSat
		}

		pair := &contactPair{
			a:        nodeA,
			b:        nodeB,
			ab:       lAB,
			ba:       lBA,
			windows:  pairWindows,
			fragSize: fragSize,
			baseSat:  baseSat,
		}
		schedulePairWindow(sim, pair, idA, idB)
	}

	return res, nil
}

func schedulePairWindow(sim *simulator.Simulator, pair *contactPair, idA, idB int) {
	// 跳过已经完全过期的窗口（end <= sim.Now）
	for pair.index < len(pair.windows) && pair.windows[pair.index].end <= sim.Now {
		pair.index++
	}
	if pair.index >= len(pair.windows) {
		return
	}

	window := pair.windows[pair.index]
	tag := fmt.Sprintf("%d-%d", idA, idB)

	// 若窗口已开始但尚未结束，link-up 时间修正为 sim.Now（立即触发）
	linkUpTime := window.start
	if linkUpTime < sim.Now {
		linkUpTime = sim.Now
	}

	sim.ScheduleWithTag(linkUpTime, func() {
		pair.ab.ResetWindow(linkUpTime)
		pair.ba.ResetWindow(linkUpTime)
		pair.a.AddLink(pair.ab)
		pair.b.AddLink(pair.ba)

		// 判断哪边是基站，分应对应的带宽/方向链路
		var base, sat *network.Node
		var baseSatLink *network.Link
		switch {
		case pair.a.IsBaseStation():
			base, sat, baseSatLink = pair.a, pair.b, pair.ab
		case pair.b.IsBaseStation():
			base, sat, baseSatLink = pair.b, pair.a, pair.ba
		}

		if base != nil {
			// 基站 → 卡星方向：调用注入策略
			pair.baseSat.OnBaseLinkUp(base, sat, baseSatLink, pair.fragSize)
		} else {
			// 卡星↔卡星：双向通知转发策略
			if pair.a.Scheduler != nil {
				pair.a.Scheduler.OnLinkUp(pair.a, pair.ab)
			}
			if pair.b.Scheduler != nil {
				pair.b.Scheduler.OnLinkUp(pair.b, pair.ba)
			}
		}
	}, "link-up:"+tag)

	sim.ScheduleWithTag(window.end, func() {
		pair.a.RemoveLink(pair.ab)
		pair.b.RemoveLink(pair.ba)
		pair.index++
		schedulePairWindow(sim, pair, idA, idB)
	}, "link-down:"+tag)
}

// timeUnitToNs 将时间单位字符串转换为纳秒乘数
func timeUnitToNs(unit string) float64 {
	switch unit {
	case "s", "":
		return float64(simulator.Second)
	case "ms":
		return float64(simulator.Millisecond)
	case "us":
		return float64(simulator.Microsecond)
	case "ns":
		return float64(simulator.Nanosecond)
	default:
		return float64(simulator.Second)
	}
}

// PrintDynamicSummary 打印动态拓扑加载摘要
func PrintDynamicSummary(res *DynamicLoadResult) {
	fmt.Printf("=== Dynamic Topology Summary ===\n")
	fmt.Printf("  Source file:    loaded from file\n")
	fmt.Printf("  Nodes:          %d total (%d satellites + 1 base)\n",
		len(res.AllNodes), len(res.Satellites))
	fmt.Printf("  Time unit:      %s\n", func() string {
		if res.Meta.TimeUnit == "" {
			return "s (default)"
		}
		return res.Meta.TimeUnit
	}())
	fmt.Printf("  Contact pairs:  %d unique link pairs with windows\n", len(res.Links)/2)
	fmt.Printf("  Total windows:  %d (each = link-up + link-down event)\n", res.TotalWindows)
	fmt.Printf("  Sim events pre-scheduled: %d initial link events\n", len(res.Links))
	fmt.Println()
}

// Package topology 负责根据配置构建网络拓扑。
package topology

import (
	"fmt"
	"sat-sim/config"
	"sat-sim/network"
	"sat-sim/protocol"
	"sat-sim/simulator"
)

// BuildResult 包含构建好的网络拓扑
type BuildResult struct {
	Base       *network.Node
	Satellites []*network.Node
	AllNodes   []*network.Node // base + satellites
	Links      []*network.Link
}

// Build 根据配置创建完整的网络拓扑
func Build(sim *simulator.Simulator, cfg config.Config) *BuildResult {
	linkIDCounter := 0
	nextLinkID := func() int {
		id := linkIDCounter
		linkIDCounter++
		return id
	}

	result := &BuildResult{}

	// 创建基站
	base := network.NewNode(0, true, sim)
	base.Storage = protocol.NewSetStorage()
	// 基站拥有所有分片
	for i := 0; i < cfg.NumFragments; i++ {
		base.Storage.Store(i)
	}
	result.Base = base
	result.AllNodes = append(result.AllNodes, base)

	// 创建卫星
	satellites := make([]*network.Node, cfg.NumSatellites)
	for i := 0; i < cfg.NumSatellites; i++ {
		sat := network.NewNode(i+1, false, sim)
		sat.Storage = protocol.NewSetStorage()
		satellites[i] = sat
		result.AllNodes = append(result.AllNodes, sat)
	}
	result.Satellites = satellites

	baseDelay := simulator.Time(cfg.BaseSatDelay)
	satDelay := simulator.Time(cfg.SatSatDelay)

	// 基站 → 每个卫星（单向：基站注入用）
	for _, sat := range satellites {
		link := network.NewLink(nextLinkID(), base, sat, cfg.BaseSatBandwidth, baseDelay, sim)
		base.AddLink(link)
		result.Links = append(result.Links, link)
	}

	// 卫星间拓扑
	switch cfg.Topology {
	case "full_mesh":
		// 所有卫星两两双向互联
		for i := 0; i < len(satellites); i++ {
			for j := i + 1; j < len(satellites); j++ {
				l1 := network.NewLink(nextLinkID(), satellites[i], satellites[j], cfg.SatSatBandwidth, satDelay, sim)
				l2 := network.NewLink(nextLinkID(), satellites[j], satellites[i], cfg.SatSatBandwidth, satDelay, sim)
				satellites[i].AddLink(l1)
				satellites[j].AddLink(l2)
				result.Links = append(result.Links, l1, l2)
			}
		}

	case "ring":
		// 卫星按编号组成环
		n := len(satellites)
		for i := 0; i < n; i++ {
			next := (i + 1) % n
			l1 := network.NewLink(nextLinkID(), satellites[i], satellites[next], cfg.SatSatBandwidth, satDelay, sim)
			l2 := network.NewLink(nextLinkID(), satellites[next], satellites[i], cfg.SatSatBandwidth, satDelay, sim)
			satellites[i].AddLink(l1)
			satellites[next].AddLink(l2)
			result.Links = append(result.Links, l1, l2)
		}

	case "star":
		// 仅基站↔卫星，无卫星间链路
		// 需要额外添加卫星→基站的反向链路用于中转
		for _, sat := range satellites {
			link := network.NewLink(nextLinkID(), sat, base, cfg.BaseSatBandwidth, baseDelay, sim)
			sat.AddLink(link)
			result.Links = append(result.Links, link)
		}

	default:
		panic(fmt.Sprintf("unknown topology: %s", cfg.Topology))
	}

	return result
}

// SetSchedulers 为所有卫星节点设置调度策略
func SetSchedulers(satellites []*network.Node, scheduler protocol.SchedulingStrategy) {
	for _, sat := range satellites {
		sat.Scheduler = scheduler
	}
}

// PrintTopology 打印拓扑摘要
func PrintTopology(result *BuildResult) {
	fmt.Printf("=== Topology Summary ===\n")
	fmt.Printf("  Base station: Node 0\n")
	fmt.Printf("  Satellites: %d nodes (ID 1-%d)\n", len(result.Satellites), len(result.Satellites))
	fmt.Printf("  Total links: %d\n", len(result.Links))
	for _, node := range result.AllNodes {
		links := node.GetLinks()
		fmt.Printf("  Node %d (%s): %d outgoing links → ",
			node.NodeID(),
			nodeType(node),
			len(links))
		for i, l := range links {
			if i > 0 {
				fmt.Print(", ")
			}
			fmt.Printf("Node %d", l.Destination().NodeID())
		}
		fmt.Println()
	}
	fmt.Println()
}

func nodeType(n *network.Node) string {
	if n.IsBaseStation() {
		return "base"
	}
	return "sat"
}

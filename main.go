package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"sat-sim/config"
	"sat-sim/network"
	"sat-sim/protocol"
	"sat-sim/simulator"
	"sat-sim/topology"
)

func main() {
	// --- 命令行参数 ---
	serveMode := flag.Bool("serve", false, "启动 HTTP API 服务")
	listenAddr := flag.String("addr", ":8080", "HTTP API 监听地址")
	topoDir := flag.String("topo-dir", "topofile", "拓扑文件目录（API 模式使用）")
	configFile := flag.String("config", "", "JSON 配置文件路径（可选）")
	genConfig := flag.Bool("gen-config", false, "生成默认配置文件 config.json 并退出")
	topoFile := flag.String("topo-file", "", "动态拓扑文件路径（JSON）；设置后忽略 -topology/-satellites")
	maxSatLinks := flag.Int("max-sat-links", -1, "每颗卫星最大同时建立链路数（动态拓扑，-1 使用配置值，0 表示不限制）")

	// 静态拓扑参数（可覆盖配置文件）
	numSat := flag.Int("satellites", 0, "卫星数量（静态拓扑）")
	numFrag := flag.Int("fragments", 0, "分片数量")
	fragSize := flag.Int("frag-size", 0, "分片大小/字节")
	topoType := flag.String("topology", "", "静态拓扑: full_mesh, ring, star")
	schedType := flag.String("scheduler", "", "调度策略: epidemic, gossip, push_pull, satdissem, rlnc, fl_gossip")

	flag.Parse()

	// --- 生成默认配置 ---
	if *genConfig {
		cfg := config.DefaultConfig()
		if err := cfg.SaveToFile("config.json"); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Generated config.json")
		return
	}

	// --- 加载配置 ---
	var cfg config.Config
	if *configFile != "" {
		var err error
		cfg, err = config.LoadFromFile(*configFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	} else {
		cfg = config.DefaultConfig()
	}

	// 命令行覆盖
	if *numSat > 0 {
		cfg.NumSatellites = *numSat
	}
	if *numFrag > 0 {
		cfg.NumFragments = *numFrag
	}
	if *fragSize > 0 {
		cfg.FragmentSize = *fragSize
	}
	if *topoType != "" {
		cfg.Topology = *topoType
	}
	if *schedType != "" {
		cfg.SchedulerType = *schedType
	}
	if *topoFile != "" {
		cfg.TopoFile = *topoFile
	}
	if *maxSatLinks >= 0 {
		cfg.MaxConcurrentLinksPerSat = *maxSatLinks
	}

	if *serveMode {
		if err := runAPIServer(*listenAddr, *topoDir, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	fmt.Println("=== Satellite Fragment Distribution Simulator ===")
	fmt.Println(cfg)
	fmt.Println()

	// --- 创建仿真器 ---
	sim := simulator.New()

	// --- 创建调度策略 ---
	scheduler := buildScheduler(cfg)

	// --- 构建拓扑（静态 or 动态）---
	var satellites []*network.Node

	if cfg.TopoFile != "" {
		// ===== 动态拓扑路径 =====
		var opts topology.DynamicOptions

		if cfg.SchedulerType == "satdissem" {
			// ---------- SATDISSEM 完整算法 ----------
			const planeSize = 32
			stats := protocol.NewSimpleInjectionStats(cfg.NumFragments)
			// groupMap 在 LoadDynamic 后填充，仿真事件在 sim.Run() 后才触发，时序上没问题
			groupMap := make(map[int][]protocol.NodeInfo)
			opts = topology.DynamicOptions{
				// 每个卫星节点独立创建 ScarcityGossip 实例（独立 scarcityCount）
				NewScheduler: func() protocol.SchedulingStrategy {
					return protocol.NewScarcityGossip(cfg.NumFragments, cfg.FragmentSize, 64)
				},
				BaseSat: protocol.NewSmartInjection(
					stats,
					// 卫星ID从1开始，(nodeID-1)/planeSize 保证32颗卫星完整归入同一平面
					func(sat protocol.NodeInfo) int { return (sat.NodeID() - 1) / planeSize },
					func(g int) []protocol.NodeInfo { return groupMap[g] },
					0.6,
				),
				LinkKindFn: func(idA, idB, baseID int) protocol.LinkKind {
					if idA == baseID || idB == baseID {
						return protocol.LinkBaseSat
					}
					if (idA-1)/planeSize == (idB-1)/planeSize {
						return protocol.LinkIntraPlane
					}
					return protocol.LinkInterPlane
				},
			}
			_ = groupMap // 下方 LoadDynamic 后填充
			// 保存引用供后续填充
			defer func() {}() // placeholder
			_ = stats
		} else if cfg.SchedulerType == "rlnc" {
			rlncScheduler, rlncBase := buildRLNCStrategies(cfg)
			opts = topology.DynamicOptions{
				Scheduler: rlncScheduler,
				BaseSat:   rlncBase,
			}
		} else if cfg.SchedulerType == "fl_gossip" {
			opts = topology.DynamicOptions{
				Scheduler: buildFLGossipScheduler(cfg),
				LinkKindFn: func(idA, idB, baseID int) protocol.LinkKind {
					if idA == baseID || idB == baseID {
						return protocol.LinkBaseSat
					}
					if (idA-1)/cfg.FLGossipPlaneSize == (idB-1)/cfg.FLGossipPlaneSize {
						return protocol.LinkIntraPlane
					}
					return protocol.LinkInterPlane
				},
			}
		} else {
			opts = topology.DynamicOptions{Scheduler: scheduler}
		}

		res, err := topology.LoadDynamic(sim, cfg, cfg.TopoFile, opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading topo file: %v\n", err)
			os.Exit(1)
		}
		topology.PrintDynamicSummary(res)
		satellites = res.Satellites

		// SATDISSEM：用真实卫星列表填充分组映射（在 sim.Run() 前完成即可）
		if cfg.SchedulerType == "satdissem" {
			const planeSize = 32
			groupMap := make(map[int][]protocol.NodeInfo)
			for _, sat := range res.Satellites {
				g := (sat.NodeID() - 1) / planeSize
				groupMap[g] = append(groupMap[g], sat)
			}
			if si, ok := opts.BaseSat.(*protocol.SmartInjection); ok {
				si.GroupPeers = func(g int) []protocol.NodeInfo { return groupMap[g] }
			}
			fmt.Printf("=== SATDISSEM: %d orbital groups (planeSize=%d) ===\n",
				len(groupMap), planeSize)
		}

		// 动态拓扑：注入由 link-up 事件触发，不需要静态注入
		fmt.Printf("=== Dynamic injection: fragments will be pushed on link-up events ===\n\n")
	} else {
		// ===== 静态拓扑路径 =====
		net := topology.Build(sim, cfg)
		topology.PrintTopology(net)
		topology.SetSchedulers(net.Satellites, scheduler)
		satellites = net.Satellites

		if cfg.SchedulerType == "rlnc" {
			_, rlncBase := buildRLNCStrategies(cfg)
			fmt.Println("=== RLNC coded injection from base station ===")
			for _, link := range net.Base.GetLinks() {
				rlncBase.OnBaseLinkUp(net.Base, link.Destination(), link, cfg.FragmentSize)
			}
			fmt.Printf("  Sent RLNC coded symbols via %d base links\n\n", len(net.Base.GetLinks()))
		} else {
			// 静态注入
			injector := buildInjector(cfg)
			fragments := make([]int, cfg.NumFragments)
			for i := range fragments {
				fragments[i] = i
			}
			satNodes := make([]protocol.NodeInfo, len(satellites))
			for i, s := range satellites {
				satNodes[i] = s
			}
			fmt.Println("=== Injecting fragments from base station ===")
			injector.Inject(net.Base, satNodes, fragments, cfg.FragmentSize)
			fmt.Printf("  Injected %d fragments into %d satellites\n\n", cfg.NumFragments, cfg.NumSatellites)
		}
	}

	var completionTime simulator.Time
	completed := false
	completedSatellites := 0
	for _, sat := range satellites {
		sat.OnNewFragment = func(node *network.Node, pkt protocol.Packet) {
			if completed {
				return
			}
			if node.Storage.Count() == cfg.NumFragments {
				completedSatellites++
				if completedSatellites == len(satellites) {
					completed = true
					completionTime = sim.Now
					sim.Stop()
				}
			}
		}
	}

	// --- 进度回调 ---
	sim.ProgressInterval = 50000
	sim.OnProgress = func(s *simulator.Simulator) {
		totalOwned := 0
		totalNeeded := len(satellites) * cfg.NumFragments
		for _, sat := range satellites {
			totalOwned += sat.Storage.Count()
		}
		fmt.Printf("  [Progress] time=%s, events=%d, pending=%d, coverage=%.1f%%\n",
			simulator.FormatTime(s.Now),
			s.EventCount(),
			s.PendingEvents(),
			float64(totalOwned)/float64(totalNeeded)*100,
		)
	}

	// --- 运行仿真 ---
	fmt.Println("=== Running simulation ===")
	wallStart := time.Now()
	sim.Run()
	wallElapsed := time.Since(wallStart)

	// --- 输出结果 ---
	fmt.Println()
	fmt.Println("=== Simulation Results ===")

	if completed {
		fmt.Printf("  Status: COMPLETE - all %d satellites have all %d fragments\n",
			len(satellites), cfg.NumFragments)
		fmt.Printf("  Completion time (logical): %s\n", simulator.FormatTime(completionTime))
	} else {
		fmt.Printf("  Status: INCOMPLETE - simulation ended before all satellites got all fragments\n")
		fmt.Printf("  Final sim time (logical): %s\n", simulator.FormatTime(sim.Now))
	}

	fmt.Printf("  Total events processed: %d\n", sim.EventCount())
	fmt.Printf("  Wall-clock time: %v\n", wallElapsed)
	fmt.Println()

	// 每个卫星的统计
	fmt.Println("=== Per-Satellite Statistics ===")
	fmt.Printf("  %-10s %-12s %-12s %-12s %-10s\n",
		"Satellite", "Owned", "Total", "Recv(all)", "Recv(new)")
	totalRecv := 0
	totalNewRecv := 0
	for _, sat := range satellites {
		fmt.Printf("  %-10d %-12d %-12d %-12d %-10d\n",
			sat.NodeID(),
			sat.Storage.Count(),
			cfg.NumFragments,
			sat.RecvCount,
			sat.NewRecvCount,
		)
		totalRecv += sat.RecvCount
		totalNewRecv += sat.NewRecvCount
	}
	fmt.Printf("  %-10s %-12s %-12s %-12d %-10d\n",
		"TOTAL", "", "", totalRecv, totalNewRecv)

	if totalRecv > 0 {
		redundancy := float64(totalRecv-totalNewRecv) / float64(totalRecv) * 100
		fmt.Printf("\n  Redundancy rate: %.1f%% (duplicate packets / total received)\n", redundancy)
	}
}

// buildScheduler 根据配置构建调度策略
func buildScheduler(cfg config.Config) protocol.SchedulingStrategy {
	switch cfg.SchedulerType {
	case "epidemic":
		return protocol.NewEpidemicScheduler(cfg.NumFragments, cfg.FragmentSize)
	case "gossip":
		return protocol.NewGossipPushScheduler(cfg.NumFragments, cfg.FragmentSize, cfg.GossipFanout, cfg.RandomSeed)
	case "push_pull":
		return protocol.NewPushPullScheduler(cfg.NumFragments, cfg.FragmentSize, cfg.GossipFanout, cfg.RandomSeed)
	case "satdissem":
		// 动态拓扑路径下由上层直接构建（含 groupMap），这里是静态拓扑的回退
		return protocol.NewScarcityGossip(cfg.NumFragments, cfg.FragmentSize, 64)
	case "rlnc":
		rlncScheduler, _ := buildRLNCStrategies(cfg)
		return rlncScheduler
	case "fl_gossip":
		return buildFLGossipScheduler(cfg)
	default:
		fmt.Fprintf(os.Stderr, "Unknown scheduler: %s\n", cfg.SchedulerType)
		os.Exit(1)
		return nil
	}
}

func buildFLGossipScheduler(cfg config.Config) protocol.SchedulingStrategy {
	return protocol.NewFLGossipScheduler(
		cfg.NumFragments,
		cfg.FragmentSize,
		cfg.FLGossipPlaneSize,
		cfg.FLGossipIntraRounds,
		cfg.FLGossipRounds,
		cfg.FLGossipFanout,
		cfg.FLGossipLossProb,
		cfg.FLGossipLocalSteps,
		cfg.FLGossipLocalStepCostUs,
		cfg.FLGossipLocalComputeOps,
		cfg.RandomSeed,
	)
}

func buildRLNCStrategies(cfg config.Config) (protocol.SchedulingStrategy, protocol.BaseSatStrategy) {
	scheduler := protocol.NewRLNCScheduler(
		cfg.NumFragments,
		cfg.FragmentSize,
		cfg.RandomSeed,
		cfg.RLNCDecodeUnitUs,
		cfg.RLNCSymbolBurst,
	)
	baseSat := protocol.NewRLNCBaseSatWithBurst(cfg.NumFragments, cfg.FragmentSize, cfg.RLNCSymbolBurst)
	return scheduler, baseSat
}

// buildInjector 根据配置构建注入策略（仅静态拓扑使用）
func buildInjector(cfg config.Config) protocol.InjectionStrategy {
	switch cfg.InjectionType {
	case "round_robin":
		return protocol.NewRoundRobinInjection()
	case "spread":
		return protocol.NewSpreadInjection()
	default:
		fmt.Fprintf(os.Stderr, "Unknown injection strategy: %s\n", cfg.InjectionType)
		os.Exit(1)
		return nil
	}
}

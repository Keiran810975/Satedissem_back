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
	configFile := flag.String("config", "", "JSON 配置文件路径（可选）")
	genConfig := flag.Bool("gen-config", false, "生成默认配置文件 config.json 并退出")
	topoFile := flag.String("topo-file", "", "动态拓扑文件路径（JSON）；设置后忽略 -topology/-satellites")

	// 静态拓扑参数（可覆盖配置文件）
	numSat := flag.Int("satellites", 0, "卫星数量（静态拓扑）")
	numFrag := flag.Int("fragments", 0, "分片数量")
	fragSize := flag.Int("frag-size", 0, "分片大小/字节")
	topoType := flag.String("topology", "", "静态拓扑: full_mesh, ring, star")
	schedType := flag.String("scheduler", "", "调度策略: epidemic, gossip, push_pull")

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
		opts := topology.DynamicOptions{
			Scheduler: scheduler,
			// BaseSat:    &protocol.PushAllBaseSat{},           // 默认，可替换为自定义注入策略
			// NewChannel: func(id int, bw float64) protocol.ChannelStrategy {
			//     return protocol.NewFIFOChannel()              // 默认，可替换为 TDMAChannel 等
			// },
		}
		res, err := topology.LoadDynamic(sim, cfg, cfg.TopoFile, opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading topo file: %v\n", err)
			os.Exit(1)
		}
		topology.PrintDynamicSummary(res)
		satellites = res.Satellites
		// 动态拓扑：注入由 link-up 事件触发，不需要静态注入
		fmt.Printf("=== Dynamic injection: fragments will be pushed on link-up events ===\n\n")
	} else {
		// ===== 静态拓扑路径 =====
		net := topology.Build(sim, cfg)
		topology.PrintTopology(net)
		topology.SetSchedulers(net.Satellites, scheduler)
		satellites = net.Satellites

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
	default:
		fmt.Fprintf(os.Stderr, "Unknown scheduler: %s\n", cfg.SchedulerType)
		os.Exit(1)
		return nil
	}
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

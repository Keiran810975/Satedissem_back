// Package config 提供仿真参数配置。
// 所有可调参数集中管理，方便修改和实验。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"sat-sim/simulator"
)

// Config 仿真配置参数
type Config struct {
	// --- 拓扑参数 ---
	NumSatellites int `json:"num_satellites"` // 卫星数量
	NumFragments  int `json:"num_fragments"`  // 分片总数
	FragmentSize  int `json:"fragment_size"`  // 每个分片大小（字节）

	// --- 链路参数 ---
	BaseSatBandwidth float64 `json:"base_sat_bandwidth"` // 基站→卫星带宽 (bps)
	SatSatBandwidth  float64 `json:"sat_sat_bandwidth"`  // 卫星↔卫星带宽 (bps)
	BaseSatDelay     uint64  `json:"base_sat_delay_ns"`  // 基站→卫星传播延迟 (ns)
	SatSatDelay      uint64  `json:"sat_sat_delay_ns"`   // 卫星↔卫星传播延迟 (ns)

	// --- 拓扑连接模式 ---
	// "full_mesh" — 所有卫星两两互联
	// "ring"      — 卫星按编号首尾相连成环
	// "star"      — 只有基站↔卫星链路，无卫星间链路（需靠基站中转）
	Topology string `json:"topology"`

	// --- 策略选择 ---
	// "epidemic"   — 收到新分片时向所有邻居推送
	// "gossip"     — 收到新分片时随机选 K 个邻居推送
	// "push_pull"  — gossip push + 周期 pull
	SchedulerType string `json:"scheduler_type"`
	GossipFanout  int    `json:"gossip_fanout"` // gossip/push_pull 的扇出度

	// "round_robin" — 轮询注入
	InjectionType string `json:"injection_type"`

	// --- 动态拓扑 ---
	// 如果设置了 TopoFile，则导入动态拓扑，忽畑6静态 Topology/NumSatellites 设置
	TopoFile string `json:"topo_file"` // 拓扑文件路径（空 = 使用静态拓扑）

	// --- 其他 ---
	RandomSeed int64 `json:"random_seed"`
}

// DefaultConfig 返回合理的默认配置
func DefaultConfig() Config {
	return Config{
		NumSatellites:    5,
		NumFragments:     20,
		FragmentSize:     1024 * 1024,                        // 1 MB
		BaseSatBandwidth: 100e6,                              // 100 Mbps
		SatSatBandwidth:  50e6,                               // 50 Mbps
		BaseSatDelay:     uint64(10 * simulator.Millisecond), // 10 ms
		SatSatDelay:      uint64(5 * simulator.Millisecond),  // 5 ms
		Topology:         "full_mesh",
		SchedulerType:    "epidemic",
		GossipFanout:     2,
		InjectionType:    "round_robin",
		RandomSeed:       42,
	}
}

// LoadFromFile 从 JSON 文件加载配置
func LoadFromFile(path string) (Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("cannot read config file %s: %w", path, err)
	}

	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("cannot parse config file %s: %w", path, err)
	}

	return cfg, nil
}

// SaveToFile 将配置写入 JSON 文件
func (c Config) SaveToFile(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// String 返回配置的可读摘要
func (c Config) String() string {
	return fmt.Sprintf(
		"Config{satellites=%d, fragments=%d, fragSize=%d B, "+
			"baseBW=%.0f bps, satBW=%.0f bps, "+
			"baseDelay=%s, satDelay=%s, "+
			"topology=%s, scheduler=%s, injection=%s}",
		c.NumSatellites, c.NumFragments, c.FragmentSize,
		c.BaseSatBandwidth, c.SatSatBandwidth,
		simulator.FormatTime(simulator.Time(c.BaseSatDelay)),
		simulator.FormatTime(simulator.Time(c.SatSatDelay)),
		c.Topology, c.SchedulerType, c.InjectionType,
	)
}

//go:build ignore
// +build ignore

// 运行方式：go run tools/convert_topo.go <input.json> <output.json>
// 将旧格式（裸对象）转换为新格式（带 meta 包装，去除空条目）
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: go run tools/convert_topo.go <input.json> <output.json>")
		os.Exit(1)
	}

	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// 解析旧格式
	var raw map[string][][2]float64
	if err := json.Unmarshal(data, &raw); err != nil {
		fmt.Fprintln(os.Stderr, "parse error:", err)
		os.Exit(1)
	}

	// 推断节点总数
	maxID := 0
	for key := range raw {
		for _, p := range strings.Split(key, "-") {
			id, e := strconv.Atoi(p)
			if e == nil && id > maxID {
				maxID = id
			}
		}
	}

	// 构造新格式（过滤空条目）
	type NewFmt struct {
		Meta struct {
			NumNodes int    `json:"num_nodes"`
			BaseNode int    `json:"base_node"`
			TimeUnit string `json:"time_unit"`
		} `json:"meta"`
		Intervals map[string][][2]float64 `json:"intervals"`
	}

	out := NewFmt{}
	out.Meta.NumNodes = maxID + 1
	out.Meta.BaseNode = 0
	out.Meta.TimeUnit = "s"
	out.Intervals = make(map[string][][2]float64)

	for k, v := range raw {
		if len(v) > 0 {
			out.Intervals[k] = v
		}
	}

	// 排序输出（可选，方便人类阅读）
	type kv struct {
		key string
		v   [][2]float64
	}
	var pairs []kv
	for k, v := range out.Intervals {
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool {
		ai := strings.Split(pairs[i].key, "-")
		bi := strings.Split(pairs[j].key, "-")
		a0, _ := strconv.Atoi(ai[0])
		b0, _ := strconv.Atoi(bi[0])
		if a0 != b0 {
			return a0 < b0
		}
		a1, _ := strconv.Atoi(ai[1])
		b1, _ := strconv.Atoi(bi[1])
		return a1 < b1
	})

	// 用有序输出重建 intervals（json.Marshal 本身不保证 map 顺序）
	orderedIntervals := make(map[string][][2]float64, len(pairs))
	_ = orderedIntervals // 直接用 pairs marshal
	_ = pairs

	result, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if err := os.WriteFile(os.Args[2], result, 0644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	nonEmpty := len(out.Intervals)
	total := len(raw)
	fmt.Printf("Done: %d total entries → %d non-empty entries (removed %d empty)\n",
		total, nonEmpty, total-nonEmpty)
	fmt.Printf("num_nodes=%d, base_node=0, time_unit=s\n", out.Meta.NumNodes)
}

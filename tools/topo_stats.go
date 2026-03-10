//go:build ignore

package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type metaInfo struct {
	NumNodes int    `json:"num_nodes"`
	BaseNode int    `json:"base_node"`
	TimeUnit string `json:"time_unit"`
}

type newFormat struct {
	Meta      metaInfo                `json:"meta"`
	Intervals map[string][][2]float64 `json:"intervals"`
}

func loadIntervals(path string) (metaInfo, map[string][][2]float64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return metaInfo{}, nil, err
	}

	var nf newFormat
	if err := json.Unmarshal(data, &nf); err == nil && nf.Intervals != nil {
		if nf.Meta.TimeUnit == "" {
			nf.Meta.TimeUnit = "s"
		}
		return nf.Meta, nf.Intervals, nil
	}

	var raw map[string][][2]float64
	if err := json.Unmarshal(data, &raw); err != nil {
		return metaInfo{}, nil, err
	}

	maxID := 0
	for key := range raw {
		parts := strings.Split(key, "-")
		for _, part := range parts {
			id, err := strconv.Atoi(part)
			if err == nil && id > maxID {
				maxID = id
			}
		}
	}

	return metaInfo{NumNodes: maxID + 1, BaseNode: 0, TimeUnit: "s"}, raw, nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: go run tools/topo_stats.go <file1.json> [file2.json ...]")
		os.Exit(1)
	}

	type row struct {
		Name              string
		Nodes             int
		Satellites        int
		NonEmptyPairs     int
		TotalWindows      int
		FirstStart        float64
		LastEnd           float64
		AvgWindowsPerPair float64
		AvgWindowDur      float64 // 平均窗口时长 (s)
		MinWindowDur      float64 // 最短窗口时长 (s)
		MaxWindowDur      float64 // 最长窗口时长 (s)
	}

	rows := make([]row, 0, len(os.Args)-1)
	for _, path := range os.Args[1:] {
		meta, intervals, err := loadIntervals(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			os.Exit(1)
		}

		nonEmptyPairs := 0
		totalWindows := 0
		firstStart := math.MaxFloat64
		lastEnd := 0.0
		sumDur := 0.0
		minDur := math.MaxFloat64
		maxDur := 0.0

		for _, windows := range intervals {
			if len(windows) == 0 {
				continue
			}
			nonEmptyPairs++
			totalWindows += len(windows)
			for _, w := range windows {
				if w[0] < firstStart {
					firstStart = w[0]
				}
				if w[1] > lastEnd {
					lastEnd = w[1]
				}
				dur := w[1] - w[0]
				if dur > 0 {
					sumDur += dur
					if dur < minDur {
						minDur = dur
					}
					if dur > maxDur {
						maxDur = dur
					}
				}
			}
		}
		if firstStart == math.MaxFloat64 {
			firstStart = 0
		}
		if minDur == math.MaxFloat64 {
			minDur = 0
		}
		avgDur := 0.0
		if totalWindows > 0 {
			avgDur = sumDur / float64(totalWindows)
		}

		rows = append(rows, row{
			Name:              filepath.Base(path),
			Nodes:             meta.NumNodes,
			Satellites:        meta.NumNodes - 1,
			NonEmptyPairs:     nonEmptyPairs,
			TotalWindows:      totalWindows,
			FirstStart:        firstStart,
			LastEnd:           lastEnd,
			AvgWindowsPerPair: float64(totalWindows) / math.Max(float64(nonEmptyPairs), 1),
			AvgWindowDur:      avgDur,
			MinWindowDur:      minDur,
			MaxWindowDur:      maxDur,
		})
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].Satellites < rows[j].Satellites })

	fmt.Printf("%-22s %6s %12s %12s %10s %12s %12s %12s\n",
		"file", "sats", "nonemptyPairs", "totalWindows", "avgWin", "avgDur(s)", "minDur(s)", "maxDur(s)")
	for _, r := range rows {
		fmt.Printf("%-22s %6d %12d %12d %10.1f %12.1f %12.1f %12.1f\n",
			r.Name, r.Satellites, r.NonEmptyPairs, r.TotalWindows,
			r.AvgWindowsPerPair, r.AvgWindowDur, r.MinWindowDur, r.MaxWindowDur)
	}
}

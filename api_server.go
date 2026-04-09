package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"sat-sim/config"
	"sat-sim/network"
	"sat-sim/protocol"
	"sat-sim/simulator"
	"sat-sim/topology"
)

type apiServer struct {
	topologyDir   string
	defaultConfig config.Config
}

type apiTopologyMeta struct {
	NumNodes int    `json:"num_nodes"`
	BaseNode int    `json:"base_node"`
	TimeUnit string `json:"time_unit"`
}

type apiTopologyFile struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
}

type apiLink struct {
	From      int          `json:"from"`
	To        int          `json:"to"`
	Intervals [][2]float64 `json:"intervals"`
}

type apiTopologyPayload struct {
	Mode  string          `json:"mode"`
	File  string          `json:"file,omitempty"`
	Meta  apiTopologyMeta `json:"meta"`
	Links []apiLink       `json:"links"`
}

type apiOptionsResponse struct {
	DefaultConfig         config.Config     `json:"default_config"`
	SchedulerOptions      []string          `json:"scheduler_options"`
	InjectionOptions      []string          `json:"injection_options"`
	StaticTopologyOptions []string          `json:"static_topology_options"`
	TopologyFiles         []apiTopologyFile `json:"topology_files"`
	DefaultTopologyFile   string            `json:"default_topology_file,omitempty"`
}

type apiTopologyResponse struct {
	File  string          `json:"file"`
	Meta  apiTopologyMeta `json:"meta"`
	Links []apiLink       `json:"links"`
}

type apiSimulateRequest struct {
	Config   *config.Config `json:"config"`
	TopoFile string         `json:"topo_file"`
}

type apiDeliveryEvent struct {
	TimeSec    float64 `json:"time_sec"`
	SrcID      int     `json:"src_id"`
	DstID      int     `json:"dst_id"`
	FragmentID int     `json:"fragment_id"`
}

type apiNodeSnapshot struct {
	ID              int    `json:"id"`
	Type            string `json:"type"`
	FinalShardCount int    `json:"final_shard_count"`
}

type apiSimulationSummary struct {
	Completed         bool     `json:"completed"`
	CompletionTimeSec *float64 `json:"completion_time_sec,omitempty"`
	FinalTimeSec      float64  `json:"final_time_sec"`
	EventCount        uint64   `json:"event_count"`
	TotalDeliveries   int      `json:"total_deliveries"`
	Satellites        int      `json:"satellites"`
	TotalFragments    int      `json:"total_fragments"`
}

type apiRLNCNodeStats struct {
	NodeID     int      `json:"node_id"`
	Rank       int      `json:"rank"`
	Decoded    bool     `json:"decoded"`
	DecodeTime *float64 `json:"decode_time_sec,omitempty"`
	CodedSent  int      `json:"coded_sent"`
	CodedRecv  int      `json:"coded_recv"`
}

type apiRLNCStats struct {
	ComplexityModel string             `json:"complexity_model"`
	DecodeUnitUs    float64            `json:"decode_unit_us"`
	SymbolBurst     int                `json:"symbol_burst"`
	Nodes           []apiRLNCNodeStats `json:"nodes"`
}

type apiFLGossipNodeStats struct {
	NodeID             int    `json:"node_id"`
	IntraSyncSent      int    `json:"intra_sync_sent"`
	InterSent          int    `json:"inter_sent"`
	InterDropped       int    `json:"inter_dropped"`
	CompensationEvents int    `json:"compensation_events"`
	IntraRoundsRun     int    `json:"intra_rounds_run"`
	InterRoundsRun     int    `json:"inter_rounds_run"`
	TrainingReady      bool   `json:"training_ready"`
	TrainingReadyAtNs  uint64 `json:"training_ready_at_ns"`
}

type apiFLGossipStats struct {
	Model           string                 `json:"model"`
	PlaneSize       int                    `json:"plane_size"`
	IntraRounds     int                    `json:"intra_rounds"`
	InterRounds     int                    `json:"inter_rounds"`
	InterFanout     int                    `json:"inter_fanout"`
	LossProb        float64                `json:"loss_prob"`
	LocalSteps      int                    `json:"local_steps"`
	LocalStepCostUs float64                `json:"local_step_cost_us"`
	LocalComputeOps int                    `json:"local_compute_ops"`
	Nodes           []apiFLGossipNodeStats `json:"nodes"`
}

type apiSimulationResponse struct {
	Config     config.Config        `json:"config"`
	Topology   apiTopologyPayload   `json:"topology"`
	Nodes      []apiNodeSnapshot    `json:"nodes"`
	Deliveries []apiDeliveryEvent   `json:"deliveries"`
	Summary    apiSimulationSummary `json:"summary"`
	RLNC       *apiRLNCStats        `json:"rlnc,omitempty"`
	FLGossip   *apiFLGossipStats    `json:"fl_gossip,omitempty"`
}

type dynamicTopologyFile struct {
	Meta      apiTopologyMeta         `json:"meta"`
	Intervals map[string][][2]float64 `json:"intervals"`
}

func runAPIServer(addr, topologyDir string, defaultCfg config.Config) error {
	absTopoDir, err := filepath.Abs(topologyDir)
	if err != nil {
		return fmt.Errorf("resolve topology dir failed: %w", err)
	}

	if stat, statErr := os.Stat(absTopoDir); statErr != nil || !stat.IsDir() {
		if statErr != nil {
			return fmt.Errorf("topology dir not accessible: %w", statErr)
		}
		return fmt.Errorf("topology dir is not a directory: %s", absTopoDir)
	}

	s := &apiServer{topologyDir: absTopoDir, defaultConfig: defaultCfg}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/options", s.handleOptions)
	mux.HandleFunc("/api/topology/files", s.handleTopologyFiles)
	mux.HandleFunc("/api/topology", s.handleTopology)
	mux.HandleFunc("/api/simulate", s.handleSimulate)

	fmt.Printf("=== API mode ===\n")
	fmt.Printf("  Listening: %s\n", addr)
	fmt.Printf("  Topology dir: %s\n\n", absTopoDir)

	return http.ListenAndServe(addr, withCORS(mux))
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func (s *apiServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
	})
}

func (s *apiServer) handleOptions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	files, err := s.listTopologyFiles()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	defaultTopo := ""
	if s.defaultConfig.TopoFile != "" {
		defaultTopo = filepath.Base(s.defaultConfig.TopoFile)
	}
	if defaultTopo == "" && len(files) > 0 {
		defaultTopo = files[0].Name
	}

	writeJSON(w, http.StatusOK, apiOptionsResponse{
		DefaultConfig:         s.defaultConfig,
		SchedulerOptions:      []string{"epidemic", "gossip", "push_pull", "satdissem", "rlnc", "fl_gossip"},
		InjectionOptions:      []string{"round_robin", "spread"},
		StaticTopologyOptions: []string{"full_mesh", "ring", "star"},
		TopologyFiles:         files,
		DefaultTopologyFile:   defaultTopo,
	})
}

func (s *apiServer) handleTopologyFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	files, err := s.listTopologyFiles()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

func (s *apiServer) handleTopology(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	fileName := r.URL.Query().Get("file")
	if fileName == "" {
		writeError(w, http.StatusBadRequest, "missing query param: file")
		return
	}

	absPath, err := s.resolveTopologyPath(fileName)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	meta, intervals, err := loadTopologyFile(absPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, apiTopologyResponse{
		File:  filepath.Base(absPath),
		Meta:  meta,
		Links: intervalsToLinks(intervals),
	})
}

func (s *apiServer) handleSimulate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req apiSimulateRequest
	if r.Body != nil {
		dec := json.NewDecoder(io.LimitReader(r.Body, 20<<20))
		dec.DisallowUnknownFields()
		err := dec.Decode(&req)
		if err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	}

	cfg := s.defaultConfig
	if req.Config != nil {
		cfg = *req.Config
	}
	cfg = normalizeConfig(cfg, s.defaultConfig)

	if req.TopoFile != "" {
		cfg.TopoFile = req.TopoFile
	}
	if cfg.TopoFile != "" {
		absPath, err := s.resolveTopologyPath(cfg.TopoFile)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		cfg.TopoFile = absPath
	}

	if err := validateConfig(cfg); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	result, err := runSimulationForAPI(cfg)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *apiServer) listTopologyFiles() ([]apiTopologyFile, error) {
	entries, err := os.ReadDir(s.topologyDir)
	if err != nil {
		return nil, fmt.Errorf("read topology dir failed: %w", err)
	}

	files := make([]apiTopologyFile, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}
		files = append(files, apiTopologyFile{
			Name:      entry.Name(),
			SizeBytes: info.Size(),
		})
	}

	sort.Slice(files, func(i, j int) bool {
		if files[i].SizeBytes == files[j].SizeBytes {
			return files[i].Name < files[j].Name
		}
		return files[i].SizeBytes < files[j].SizeBytes
	})

	return files, nil
}

func (s *apiServer) resolveTopologyPath(input string) (string, error) {
	if strings.TrimSpace(input) == "" {
		return "", fmt.Errorf("empty topology file")
	}

	cleanInput := filepath.Clean(input)
	var candidate string
	if filepath.IsAbs(cleanInput) {
		candidate = cleanInput
	} else {
		candidate = filepath.Join(s.topologyDir, cleanInput)
	}

	absCandidate, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("invalid topology path")
	}

	absRoot, err := filepath.Abs(s.topologyDir)
	if err != nil {
		return "", fmt.Errorf("invalid topology root")
	}

	rootPrefix := absRoot + string(os.PathSeparator)
	if absCandidate != absRoot && !strings.HasPrefix(absCandidate, rootPrefix) {
		return "", fmt.Errorf("topology file must be inside %s", absRoot)
	}

	if strings.ToLower(filepath.Ext(absCandidate)) != ".json" {
		return "", fmt.Errorf("topology file must be .json")
	}

	if stat, statErr := os.Stat(absCandidate); statErr != nil || stat.IsDir() {
		if statErr != nil {
			return "", fmt.Errorf("topology file not found: %s", filepath.Base(cleanInput))
		}
		return "", fmt.Errorf("topology file is a directory")
	}

	return absCandidate, nil
}

func normalizeConfig(cfg, fallback config.Config) config.Config {
	if cfg.NumSatellites <= 0 {
		cfg.NumSatellites = fallback.NumSatellites
	}
	if cfg.NumFragments <= 0 {
		cfg.NumFragments = fallback.NumFragments
	}
	if cfg.FragmentSize <= 0 {
		cfg.FragmentSize = fallback.FragmentSize
	}
	if cfg.BaseSatBandwidth <= 0 {
		cfg.BaseSatBandwidth = fallback.BaseSatBandwidth
	}
	if cfg.SatSatBandwidth <= 0 {
		cfg.SatSatBandwidth = fallback.SatSatBandwidth
	}
	if cfg.BaseSatDelay == 0 {
		cfg.BaseSatDelay = fallback.BaseSatDelay
	}
	if cfg.SatSatDelay == 0 {
		cfg.SatSatDelay = fallback.SatSatDelay
	}
	if cfg.Topology == "" {
		cfg.Topology = fallback.Topology
	}
	if cfg.SchedulerType == "" {
		cfg.SchedulerType = fallback.SchedulerType
	}
	if cfg.GossipFanout <= 0 {
		cfg.GossipFanout = fallback.GossipFanout
	}
	if cfg.FLGossipPlaneSize <= 0 {
		cfg.FLGossipPlaneSize = fallback.FLGossipPlaneSize
	}
	if cfg.FLGossipIntraRounds < 0 {
		cfg.FLGossipIntraRounds = fallback.FLGossipIntraRounds
	}
	if cfg.FLGossipRounds <= 0 {
		cfg.FLGossipRounds = fallback.FLGossipRounds
	}
	if cfg.FLGossipFanout <= 0 {
		cfg.FLGossipFanout = fallback.FLGossipFanout
	}
	if cfg.FLGossipLossProb < 0 || cfg.FLGossipLossProb > 1 {
		cfg.FLGossipLossProb = fallback.FLGossipLossProb
	}
	if cfg.FLGossipLocalSteps <= 0 {
		cfg.FLGossipLocalSteps = fallback.FLGossipLocalSteps
	}
	if cfg.FLGossipLocalStepCostUs <= 0 {
		cfg.FLGossipLocalStepCostUs = fallback.FLGossipLocalStepCostUs
	}
	if cfg.FLGossipLocalComputeOps <= 0 {
		cfg.FLGossipLocalComputeOps = fallback.FLGossipLocalComputeOps
	}
	if cfg.InjectionType == "" {
		cfg.InjectionType = fallback.InjectionType
	}
	if cfg.RandomSeed == 0 {
		cfg.RandomSeed = fallback.RandomSeed
	}
	if cfg.RLNCDecodeUnitUs <= 0 {
		cfg.RLNCDecodeUnitUs = fallback.RLNCDecodeUnitUs
	}
	if cfg.RLNCSymbolBurst < 0 {
		cfg.RLNCSymbolBurst = fallback.RLNCSymbolBurst
	}
	return cfg
}

func validateConfig(cfg config.Config) error {
	if cfg.NumFragments <= 0 {
		return fmt.Errorf("num_fragments must be > 0")
	}
	if cfg.FragmentSize <= 0 {
		return fmt.Errorf("fragment_size must be > 0")
	}
	if cfg.BaseSatBandwidth <= 0 || cfg.SatSatBandwidth <= 0 {
		return fmt.Errorf("bandwidth must be > 0")
	}
	if cfg.RLNCDecodeUnitUs <= 0 {
		return fmt.Errorf("rlnc_decode_unit_us must be > 0")
	}
	if cfg.RLNCSymbolBurst < 0 {
		return fmt.Errorf("rlnc_symbol_burst must be >= 0")
	}
	if cfg.FLGossipPlaneSize <= 0 {
		return fmt.Errorf("fl_gossip_plane_size must be > 0")
	}
	if cfg.FLGossipIntraRounds < 0 {
		return fmt.Errorf("fl_gossip_intra_rounds must be >= 0")
	}
	if cfg.FLGossipRounds <= 0 {
		return fmt.Errorf("fl_gossip_rounds must be > 0")
	}
	if cfg.FLGossipFanout <= 0 {
		return fmt.Errorf("fl_gossip_fanout must be > 0")
	}
	if cfg.FLGossipLossProb < 0 || cfg.FLGossipLossProb > 1 {
		return fmt.Errorf("fl_gossip_loss_prob must be in [0,1]")
	}
	if cfg.FLGossipLocalSteps <= 0 {
		return fmt.Errorf("fl_gossip_local_steps must be > 0")
	}
	if cfg.FLGossipLocalStepCostUs <= 0 {
		return fmt.Errorf("fl_gossip_local_step_cost_us must be > 0")
	}
	if cfg.FLGossipLocalComputeOps <= 0 {
		return fmt.Errorf("fl_gossip_local_compute_ops must be > 0")
	}

	validSchedulers := map[string]struct{}{
		"epidemic":  {},
		"gossip":    {},
		"push_pull": {},
		"satdissem": {},
		"rlnc":      {},
		"fl_gossip": {},
	}
	if _, ok := validSchedulers[cfg.SchedulerType]; !ok {
		return fmt.Errorf("unsupported scheduler_type: %s", cfg.SchedulerType)
	}

	validInjections := map[string]struct{}{
		"round_robin": {},
		"spread":      {},
	}
	if _, ok := validInjections[cfg.InjectionType]; !ok {
		return fmt.Errorf("unsupported injection_type: %s", cfg.InjectionType)
	}

	if cfg.TopoFile == "" {
		if cfg.NumSatellites <= 0 {
			return fmt.Errorf("num_satellites must be > 0 in static topology mode")
		}
		validStaticTopologies := map[string]struct{}{
			"full_mesh": {},
			"ring":      {},
			"star":      {},
		}
		if _, ok := validStaticTopologies[cfg.Topology]; !ok {
			return fmt.Errorf("unsupported topology: %s", cfg.Topology)
		}
	}

	return nil
}

func runSimulationForAPI(cfg config.Config) (*apiSimulationResponse, error) {
	sim := simulator.New()
	scheduler := buildScheduler(cfg)

	var satellites []*network.Node
	var allNodes []*network.Node
	var topoPayload apiTopologyPayload
	var rlncScheduler *protocol.RLNCScheduler
	var rlncBase *protocol.RLNCBaseSat
	var flGossipScheduler *protocol.FLGossipScheduler

	if cfg.TopoFile != "" {
		meta, intervals, err := loadTopologyFile(cfg.TopoFile)
		if err != nil {
			return nil, err
		}

		topoPayload = apiTopologyPayload{
			Mode:  "dynamic",
			File:  filepath.Base(cfg.TopoFile),
			Meta:  meta,
			Links: intervalsToLinks(intervals),
		}

		var opts topology.DynamicOptions
		if cfg.SchedulerType == "satdissem" {
			const planeSize = 32
			stats := protocol.NewSimpleInjectionStats(cfg.NumFragments)
			groupMap := make(map[int][]protocol.NodeInfo)
			opts = topology.DynamicOptions{
				NewScheduler: func() protocol.SchedulingStrategy {
					return protocol.NewScarcityGossip(cfg.NumFragments, cfg.FragmentSize, 64)
				},
				BaseSat: protocol.NewSmartInjection(
					stats,
					func(sat protocol.NodeInfo) int { return (sat.NodeID() - 1) / planeSize },
					func(group int) []protocol.NodeInfo { return groupMap[group] },
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
		} else if cfg.SchedulerType == "rlnc" {
			rlncScheduling, rlncBaseStrategy := buildRLNCStrategies(cfg)
			if rs, ok := rlncScheduling.(*protocol.RLNCScheduler); ok {
				rlncScheduler = rs
			}
			if rb, ok := rlncBaseStrategy.(*protocol.RLNCBaseSat); ok {
				rlncBase = rb
			}
			opts = topology.DynamicOptions{
				Scheduler: rlncScheduling,
				BaseSat:   rlncBaseStrategy,
			}
		} else if cfg.SchedulerType == "fl_gossip" {
			if fs, ok := buildFLGossipScheduler(cfg).(*protocol.FLGossipScheduler); ok {
				flGossipScheduler = fs
			}
			opts = topology.DynamicOptions{
				Scheduler: flGossipScheduler,
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
			return nil, err
		}

		satellites = res.Satellites
		allNodes = res.AllNodes

		if cfg.SchedulerType == "satdissem" {
			const planeSize = 32
			groupMap := make(map[int][]protocol.NodeInfo)
			for _, sat := range res.Satellites {
				group := (sat.NodeID() - 1) / planeSize
				groupMap[group] = append(groupMap[group], sat)
			}
			if si, ok := opts.BaseSat.(*protocol.SmartInjection); ok {
				si.GroupPeers = func(group int) []protocol.NodeInfo { return groupMap[group] }
			}
		}
	} else {
		net := topology.Build(sim, cfg)
		topology.SetSchedulers(net.Satellites, scheduler)
		if fs, ok := scheduler.(*protocol.FLGossipScheduler); ok {
			flGossipScheduler = fs
		}

		satellites = net.Satellites
		allNodes = net.AllNodes
		topoPayload = apiTopologyPayload{
			Mode: "static",
			Meta: apiTopologyMeta{
				NumNodes: len(net.AllNodes),
				BaseNode: 0,
				TimeUnit: "s",
			},
			Links: staticLinksToAPI(net.Links),
		}

		if cfg.SchedulerType == "rlnc" {
			if rs, ok := scheduler.(*protocol.RLNCScheduler); ok {
				rlncScheduler = rs
			}
			if rlncBase == nil {
				_, rlncBaseStrategy := buildRLNCStrategies(cfg)
				if rb, ok := rlncBaseStrategy.(*protocol.RLNCBaseSat); ok {
					rlncBase = rb
				}
			}
			for _, link := range net.Base.GetLinks() {
				if rlncBase != nil {
					rlncBase.OnBaseLinkUp(net.Base, link.Destination(), link, cfg.FragmentSize)
				}
			}
		} else {
			injector := buildInjector(cfg)
			fragments := make([]int, cfg.NumFragments)
			for i := range fragments {
				fragments[i] = i
			}
			satNodes := make([]protocol.NodeInfo, len(satellites))
			for i, sat := range satellites {
				satNodes[i] = sat
			}
			injector.Inject(net.Base, satNodes, fragments, cfg.FragmentSize)
		}
	}

	deliveries := make([]apiDeliveryEvent, 0, len(satellites)*cfg.NumFragments)
	completedSatellites := 0
	completed := false
	var completionTime simulator.Time

	for _, sat := range satellites {
		sat.OnNewFragment = func(node *network.Node, pkt protocol.Packet) {
			deliveries = append(deliveries, apiDeliveryEvent{
				TimeSec:    simTimeToSeconds(sim.Now),
				SrcID:      pkt.SrcID,
				DstID:      node.NodeID(),
				FragmentID: pkt.FragmentID,
			})

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

	sim.Run()

	finalTimeSec := simTimeToSeconds(sim.Now)
	if topoPayload.Mode == "static" {
		end := finalTimeSec
		if end < 1 {
			end = 1
		}
		for i := range topoPayload.Links {
			topoPayload.Links[i].Intervals = [][2]float64{{0, end}}
		}
	}

	nodes := make([]apiNodeSnapshot, 0, len(allNodes))
	for _, node := range allNodes {
		nodeType := "satellite"
		if node.IsBaseStation() {
			nodeType = "station"
		}
		count := 0
		if node.Storage != nil {
			count = node.Storage.Count()
		}
		nodes = append(nodes, apiNodeSnapshot{
			ID:              node.NodeID(),
			Type:            nodeType,
			FinalShardCount: count,
		})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })

	summary := apiSimulationSummary{
		Completed:       completed,
		FinalTimeSec:    finalTimeSec,
		EventCount:      sim.EventCount(),
		TotalDeliveries: len(deliveries),
		Satellites:      len(satellites),
		TotalFragments:  cfg.NumFragments,
	}
	if completed {
		completion := simTimeToSeconds(completionTime)
		summary.CompletionTimeSec = &completion
	}

	var rlncPayload *apiRLNCStats
	if rlncScheduler != nil {
		rlncStats := rlncScheduler.Stats()
		nodeStats := make([]apiRLNCNodeStats, 0, len(rlncStats.Nodes))
		for _, stat := range rlncStats.Nodes {
			var decodeTimeSec *float64
			if stat.HasDecodeAt {
				t := simTimeToSeconds(stat.DecodeTime)
				decodeTimeSec = &t
			}
			nodeStats = append(nodeStats, apiRLNCNodeStats{
				NodeID:     stat.NodeID,
				Rank:       stat.Rank,
				Decoded:    stat.Decoded,
				DecodeTime: decodeTimeSec,
				CodedSent:  stat.CodedSent,
				CodedRecv:  stat.CodedRecv,
			})
		}

		symbolBurst := rlncStats.SymbolBurst
		if rlncBase != nil {
			symbolBurst = rlncBase.SymbolBurst()
		}

		rlncPayload = &apiRLNCStats{
			ComplexityModel: rlncStats.ComplexityModel,
			DecodeUnitUs:    rlncStats.DecodeUnitUs,
			SymbolBurst:     symbolBurst,
			Nodes:           nodeStats,
		}
	}

	var flPayload *apiFLGossipStats
	if flGossipScheduler != nil {
		flStats := flGossipScheduler.Stats()
		nodes := make([]apiFLGossipNodeStats, 0, len(flStats.Nodes))
		for _, stat := range flStats.Nodes {
			nodes = append(nodes, apiFLGossipNodeStats{
				NodeID:             stat.NodeID,
				IntraSyncSent:      stat.IntraSyncSent,
				InterSent:          stat.InterSent,
				InterDropped:       stat.InterDropped,
				CompensationEvents: stat.CompensationEvents,
				IntraRoundsRun:     stat.IntraRoundsRun,
				InterRoundsRun:     stat.InterRoundsRun,
				TrainingReady:      stat.TrainingReady,
				TrainingReadyAtNs:  uint64(stat.TrainingReadyAt),
			})
		}
		flPayload = &apiFLGossipStats{
			Model:           flStats.Model,
			PlaneSize:       flStats.PlaneSize,
			IntraRounds:     flStats.IntraRounds,
			InterRounds:     flStats.InterRounds,
			InterFanout:     flStats.InterFanout,
			LossProb:        flStats.LossProb,
			LocalSteps:      flStats.LocalSteps,
			LocalStepCostUs: flStats.LocalStepCostUs,
			LocalComputeOps: flStats.LocalComputeOps,
			Nodes:           nodes,
		}
	}

	return &apiSimulationResponse{
		Config:     cfg,
		Topology:   topoPayload,
		Nodes:      nodes,
		Deliveries: deliveries,
		Summary:    summary,
		RLNC:       rlncPayload,
		FLGossip:   flPayload,
	}, nil
}

func staticLinksToAPI(links []*network.Link) []apiLink {
	out := make([]apiLink, 0, len(links))
	for _, link := range links {
		out = append(out, apiLink{
			From:      link.Source().NodeID(),
			To:        link.Destination().NodeID(),
			Intervals: [][2]float64{{0, 1}},
		})
	}
	return out
}

func simTimeToSeconds(t simulator.Time) float64 {
	return float64(t) / float64(simulator.Second)
}

func loadTopologyFile(path string) (apiTopologyMeta, map[string][][2]float64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return apiTopologyMeta{}, nil, fmt.Errorf("cannot read topo file %s: %w", path, err)
	}

	var wrapped dynamicTopologyFile
	if err := json.Unmarshal(data, &wrapped); err == nil && wrapped.Intervals != nil {
		if wrapped.Meta.TimeUnit == "" {
			wrapped.Meta.TimeUnit = "s"
		}
		if wrapped.Meta.NumNodes == 0 {
			wrapped.Meta.NumNodes = inferNumNodes(wrapped.Intervals)
		}
		return wrapped.Meta, wrapped.Intervals, nil
	}

	var intervals map[string][][2]float64
	if err := json.Unmarshal(data, &intervals); err != nil {
		return apiTopologyMeta{}, nil, fmt.Errorf("cannot parse topo file %s: %w", path, err)
	}

	return apiTopologyMeta{
		NumNodes: inferNumNodes(intervals),
		BaseNode: 0,
		TimeUnit: "s",
	}, intervals, nil
}

func inferNumNodes(intervals map[string][][2]float64) int {
	maxID := 0
	for key := range intervals {
		a, b, ok := parsePairKey(key)
		if !ok {
			continue
		}
		if a > maxID {
			maxID = a
		}
		if b > maxID {
			maxID = b
		}
	}
	return maxID + 1
}

func intervalsToLinks(intervals map[string][][2]float64) []apiLink {
	keys := make([]string, 0, len(intervals))
	for key := range intervals {
		keys = append(keys, key)
	}

	sort.Slice(keys, func(i, j int) bool {
		ai, bi, okI := parsePairKey(keys[i])
		aj, bj, okJ := parsePairKey(keys[j])
		if okI && okJ {
			if ai != aj {
				return ai < aj
			}
			if bi != bj {
				return bi < bj
			}
			return keys[i] < keys[j]
		}
		if okI != okJ {
			return okI
		}
		return keys[i] < keys[j]
	})

	links := make([]apiLink, 0, len(keys))
	for _, key := range keys {
		a, b, ok := parsePairKey(key)
		if !ok {
			continue
		}
		windows := intervals[key]
		if len(windows) == 0 {
			continue
		}
		copied := make([][2]float64, len(windows))
		copy(copied, windows)
		links = append(links, apiLink{From: a, To: b, Intervals: copied})
	}

	return links
}

func parsePairKey(key string) (int, int, bool) {
	parts := strings.Split(key, "-")
	if len(parts) != 2 {
		return 0, 0, false
	}
	a, errA := strconv.Atoi(parts[0])
	b, errB := strconv.Atoi(parts[1])
	if errA != nil || errB != nil {
		return 0, 0, false
	}
	return a, b, true
}

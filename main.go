// filter_nodes downloads the latest all.json from the Ethereum discv4-dns-lists
// repository, decodes each node's ENR record, identifies which EVM-compatible
// chain the node belongs to, determines the dominant (current) fork ID per chain,
// then writes ranked JSON files of the top bootstrap peers for each configured
// chain directly into the output directory as {chain-name}.json.
//
// Supported filter strategies (filterType):
//
//   - geth_network    uses go-ethereum forkid.NewStaticFilter so it accepts every
//     node on the same chain regardless of current fork level.
//     Supported networks: mainnet, sepolia, holesky, hoodi.
//
//   - enr_field       accepts nodes that advertise a specific ENR key (e.g. "bsc"
//     for BNB Smart Chain nodes).  When forkHashes is ALSO set,
//     both conditions must match (compound AND filter).
//
//   - fork_hash_list  accepts nodes whose current eth fork hash appears in the
//     configured forkHashes list.  When enrField is ALSO set,
//     both conditions must match (compound AND filter).
//     Run with -discover to print a ranked list of all observed fork
//     hashes and identify which hash belongs to which chain.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

// ---------------------------------------------------------------------------
// Configuration types
// ---------------------------------------------------------------------------

// AppConfig is the root of chains_config.json.
type AppConfig struct {
	AllJSONURL  string        `json:"allJsonURL"`
	OutputDir   string        `json:"outputDir"`
	DefaultTopN int           `json:"defaultTopN"`
	Chains      []ChainConfig `json:"chains"`
}

// ChainConfig describes one chain to filter for.
//
// filterType values:
//   - "geth_network"   – go-ethereum forkid filter; requires "network".
//   - "enr_field"      – presence of a specific ENR key; requires "enrField".
//   - "fork_hash_list" – eth fork hash exact match; requires "forkHashes".
//
// Compound AND: set both enrField AND forkHashes together with either
// "enr_field" or "fork_hash_list" as filterType to require both conditions.
type ChainConfig struct {
	Name        string   `json:"name"`
	ChainID     int      `json:"chainId"`
	Description string   `json:"description,omitempty"`
	FilterType  string   `json:"filterType"`
	Network     string   `json:"network,omitempty"`    // geth_network
	EnrField    string   `json:"enrField,omitempty"`   // enr_field (or compound)
	ForkHashes  []string `json:"forkHashes,omitempty"` // fork_hash_list (or compound)
	TopN        int      `json:"topN,omitempty"`
}

// ---------------------------------------------------------------------------
// all.json types
// ---------------------------------------------------------------------------

// NodeRecord mirrors one entry in all.json.
type NodeRecord struct {
	Seq           uint64    `json:"seq"`
	Record        string    `json:"record"`
	Score         int       `json:"score"`
	FirstResponse time.Time `json:"firstResponse,omitempty"`
	LastResponse  time.Time `json:"lastResponse,omitempty"`
	LastCheck     time.Time `json:"lastCheck,omitempty"`
}

// ---------------------------------------------------------------------------
// Output types
// ---------------------------------------------------------------------------

// OutputNode is one entry in an output {chain}.json file.
type OutputNode struct {
	ENR          string    `json:"enr"`
	NodeID       string    `json:"nodeId"`
	Score        int       `json:"score"`
	LastResponse time.Time `json:"lastResponse,omitempty"`
	ForkID       string    `json:"forkId,omitempty"`
	ForkNext     uint64    `json:"forkNext,omitempty"`
	IP           string    `json:"ip,omitempty"`
	Port         int       `json:"port,omitempty"`
}

// ---------------------------------------------------------------------------
// Internal working types
// ---------------------------------------------------------------------------

type candidateNode struct {
	nodeID   string
	record   NodeRecord
	node     *enode.Node
	forkHash string // hex-encoded 4-byte fork hash, or "" if no eth entry
	forkNext uint64
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	configPath := flag.String("config", "chains_config.json", "path to chains_config.json")
	inputFile := flag.String("input", "", "local all.json file to use instead of downloading")
	discover := flag.Bool("discover", false, "print fork-hash discovery summary and exit")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	raw, err := loadAllJSON(*inputFile, cfg.AllJSONURL)
	if err != nil {
		log.Fatalf("load all.json: %v", err)
	}

	var allNodes map[string]NodeRecord
	if err := json.Unmarshal(raw, &allNodes); err != nil {
		log.Fatalf("parse all.json: %v", err)
	}
	log.Printf("Loaded %d nodes from all.json (SHA256=%s...)", len(allNodes), shortSHA(raw))

	if *discover {
		printDiscovery(allNodes)
		return
	}

	defaultTopN := cfg.DefaultTopN
	if defaultTopN <= 0 {
		defaultTopN = 100
	}
	outDir := cfg.OutputDir
	if outDir == "" {
		outDir = "output"
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		log.Fatalf("mkdir %s: %v", outDir, err)
	}

	chainEnodes := make(map[string][]OutputNode)
	for _, chain := range cfg.Chains {
		topN := chain.TopN
		if topN <= 0 {
			topN = defaultTopN
		}
		nodes, err := processChain(chain, allNodes, outDir, topN)
		if err != nil {
			log.Printf("ERROR processing chain %s: %v", chain.Name, err)
			continue
		}
		if nodes == nil {
			nodes = []OutputNode{}
		}
		chainEnodes[chain.Name] = nodes
	}

	// Write the combined chain_enodes.json file.
	if err := writeChainEnodes(outDir, chainEnodes); err != nil {
		log.Fatalf("write chain_enodes.json: %v", err)
	}
}

// shortSHA returns the first 16 hex characters of the SHA-256 of b,
// suitable for log messages.
func shortSHA(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])[:16]
}

// ---------------------------------------------------------------------------
// Config & download helpers
// ---------------------------------------------------------------------------

func loadConfig(path string) (*AppConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg AppConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func loadAllJSON(localFile, url string) ([]byte, error) {
	if localFile != "" {
		log.Printf("Reading all.json from local file: %s", localFile)
		return os.ReadFile(localFile)
	}
	log.Printf("Downloading all.json from %s", url)
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// ---------------------------------------------------------------------------
// Discovery mode
// ---------------------------------------------------------------------------

// printDiscovery prints a ranked summary of all fork hashes seen in the dataset
// along with any chain-specific ENR fields.  Use this to identify which fork
// hash belongs to which chain when configuring fork_hash_list entries.
func printDiscovery(allNodes map[string]NodeRecord) {
	type fhStats struct {
		count      int
		totalScore int
		extraKeys  map[string]int // chain-specific ENR keys seen alongside this hash
	}
	stats := make(map[string]*fhStats)
	for _, record := range allNodes {
		n, err := enode.Parse(enode.ValidSchemes, record.Record)
		if err != nil {
			continue
		}
		fh, _ := extractForkID(n)
		if fh == "" {
			continue
		}
		s := stats[fh]
		if s == nil {
			s = &fhStats{extraKeys: make(map[string]int)}
			stats[fh] = s
		}
		s.count++
		s.totalScore += record.Score
		// Record chain-specific ENR keys
		for _, key := range []string{"bsc", "opera", "wit", "diff", "beacon", "snap", "les"} {
			var dummy struct {
				Tail []rlp.RawValue `rlp:"tail"`
			}
			if n.Load(enr.WithEntry(key, &dummy)) == nil {
				s.extraKeys[key]++
			}
		}
	}

	// Sort by totalScore descending (same metric used for dominant fork selection).
	type row struct {
		hash  string
		stats *fhStats
	}
	rows := make([]row, 0, len(stats))
	for h, s := range stats {
		rows = append(rows, row{h, s})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].stats.totalScore != rows[j].stats.totalScore {
			return rows[i].stats.totalScore > rows[j].stats.totalScore
		}
		return rows[i].stats.count > rows[j].stats.count
	})

	fmt.Println("Fork hash discovery summary (sorted by total score; use to identify chain fork hashes):")
	fmt.Printf("%-12s %7s %12s  %s\n", "FORK_HASH", "NODES", "TOTAL_SCORE", "EXTRA_ENR_KEYS")
	for _, r := range rows {
		keys := ""
		for k, n := range r.stats.extraKeys {
			keys += fmt.Sprintf("%s(%d) ", k, n)
		}
		fmt.Printf("%-12s %7d %12d  %s\n", r.hash, r.stats.count, r.stats.totalScore, keys)
	}
}

// ---------------------------------------------------------------------------
// Core pipeline
// ---------------------------------------------------------------------------

func processChain(chain ChainConfig, allNodes map[string]NodeRecord, outputDir string, topN int) ([]OutputNode, error) {
	filter, err := buildFilter(chain)
	if err != nil {
		return nil, fmt.Errorf("build filter: %w", err)
	}

	// Step 1: collect matching candidates.
	var candidates []candidateNode
	for nodeID, record := range allNodes {
		n, err := enode.Parse(enode.ValidSchemes, record.Record)
		if err != nil {
			continue
		}
		if !filter(n) {
			continue
		}
		fh, fn := extractForkID(n)
		candidates = append(candidates, candidateNode{
			nodeID:   nodeID,
			record:   record,
			node:     n,
			forkHash: fh,
			forkNext: fn,
		})
	}

	if len(candidates) == 0 {
		log.Printf("[%s] No matching nodes found", chain.Name)
		return nil, nil
	}
	log.Printf("[%s] Matched %d nodes (all fork versions)", chain.Name, len(candidates))

	// Step 2: find the dominant fork hash (highest aggregate score).
	dominant := dominantForkHash(candidates)
	log.Printf("[%s] Dominant fork hash: %s", chain.Name, dominant)

	// Step 3: filter to dominant fork hash only.
	var filtered []candidateNode
	for _, c := range candidates {
		if c.forkHash == dominant {
			filtered = append(filtered, c)
		}
	}
	log.Printf("[%s] Nodes on dominant fork: %d", chain.Name, len(filtered))

	// Step 4: rank by score desc, then lastResponse desc.
	sort.Slice(filtered, func(i, j int) bool {
		si, sj := filtered[i].record.Score, filtered[j].record.Score
		if si != sj {
			return si > sj
		}
		return filtered[i].record.LastResponse.After(filtered[j].record.LastResponse)
	})

	// Step 5: cap at topN.
	if len(filtered) > topN {
		filtered = filtered[:topN]
	}

	// Step 6: marshal to OutputNode slice.
	output := make([]OutputNode, 0, len(filtered))
	for _, c := range filtered {
		output = append(output, toOutputNode(c))
	}

	// Step 7: write JSON atomically directly into outputDir/{chain.Name}.json.
	outPath := filepath.Join(outputDir, chain.Name+".json")
	tmpPath := outPath + ".tmp"
	data, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return nil, fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmpPath, outPath); err != nil {
		return nil, fmt.Errorf("rename: %w", err)
	}
	log.Printf("[%s] Wrote %d nodes → %s", chain.Name, len(output), outPath)
	return output, nil
}

// writeChainEnodes writes a combined chain_enodes.json file mapping each chain
// name to its list of enode records into outputDir.
func writeChainEnodes(outputDir string, chainEnodes map[string][]OutputNode) error {
	outPath := filepath.Join(outputDir, "chain_enodes.json")
	tmpPath := outPath + ".tmp"
	data, err := json.MarshalIndent(chainEnodes, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmpPath, outPath); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	log.Printf("Wrote combined chain_enodes.json → %s", outPath)
	return nil
}

// ---------------------------------------------------------------------------
// Filter construction
// ---------------------------------------------------------------------------

// nodeFilter returns true if the node belongs to the target chain.
type nodeFilter func(*enode.Node) bool

// buildFilter constructs a node filter from a ChainConfig.
//
// Compound AND behaviour: if both enrField and forkHashes are present, the
// returned filter requires BOTH conditions to match simultaneously.  This
// lets you narrow a chain-specific ENR field (e.g. "bsc") to a specific
// fork version (e.g. testnet vs mainnet).
func buildFilter(chain ChainConfig) (nodeFilter, error) {
	var filters []nodeFilter

	switch chain.FilterType {
	case "geth_network":
		f, err := buildGethFilter(chain.Network)
		if err != nil {
			return nil, err
		}
		filters = append(filters, f)
	case "enr_field":
		if chain.EnrField == "" {
			return nil, fmt.Errorf("enr_field filter requires enrField")
		}
		filters = append(filters, buildEnrFieldFilter(chain.EnrField))
	case "fork_hash_list":
		if len(chain.ForkHashes) == 0 {
			return nil, fmt.Errorf("fork_hash_list filter requires forkHashes; run -discover to find them")
		}
		filters = append(filters, buildForkHashListFilter(chain.ForkHashes))
	default:
		return nil, fmt.Errorf("unknown filterType %q", chain.FilterType)
	}

	// Compound AND: add secondary conditions when both fields are present.
	if chain.FilterType != "enr_field" && chain.EnrField != "" {
		filters = append(filters, buildEnrFieldFilter(chain.EnrField))
	}
	if chain.FilterType != "fork_hash_list" && len(chain.ForkHashes) > 0 {
		filters = append(filters, buildForkHashListFilter(chain.ForkHashes))
	}

	if len(filters) == 1 {
		return filters[0], nil
	}
	return func(n *enode.Node) bool {
		for _, f := range filters {
			if !f(n) {
				return false
			}
		}
		return true
	}, nil
}

// buildGethFilter uses go-ethereum's forkid.NewStaticFilter evaluated at genesis
// time to accept any node that is on the same chain (same genesis hash) regardless
// of which fork level they are currently at.
func buildGethFilter(network string) (nodeFilter, error) {
	var filter forkid.Filter
	switch network {
	case "mainnet":
		filter = forkid.NewStaticFilter(params.MainnetChainConfig, core.DefaultGenesisBlock().ToBlock())
	case "sepolia":
		filter = forkid.NewStaticFilter(params.SepoliaChainConfig, core.DefaultSepoliaGenesisBlock().ToBlock())
	case "holesky":
		filter = forkid.NewStaticFilter(params.HoleskyChainConfig, core.DefaultHoleskyGenesisBlock().ToBlock())
	case "hoodi":
		filter = forkid.NewStaticFilter(params.HoodiChainConfig, core.DefaultHoodiGenesisBlock().ToBlock())
	default:
		return nil, fmt.Errorf("unknown geth network %q", network)
	}
	return func(n *enode.Node) bool {
		var eth struct {
			ForkID forkid.ID
			Tail   []rlp.RawValue `rlp:"tail"`
		}
		if n.Load(enr.WithEntry("eth", &eth)) != nil {
			return false
		}
		return filter(eth.ForkID) == nil
	}, nil
}

// buildEnrFieldFilter accepts nodes that advertise a specific ENR key.
func buildEnrFieldFilter(field string) nodeFilter {
	return func(n *enode.Node) bool {
		var val struct {
			Tail []rlp.RawValue `rlp:"tail"`
		}
		return n.Load(enr.WithEntry(field, &val)) == nil
	}
}

// buildForkHashListFilter accepts nodes whose eth fork hash is in the provided list.
func buildForkHashListFilter(hashes []string) nodeFilter {
	allowed := make(map[string]bool, len(hashes))
	for _, h := range hashes {
		allowed[h] = true
	}
	return func(n *enode.Node) bool {
		fh, _ := extractForkID(n)
		return fh != "" && allowed[fh]
	}
}

// ---------------------------------------------------------------------------
// Fork ID helpers
// ---------------------------------------------------------------------------

func extractForkID(n *enode.Node) (hashHex string, next uint64) {
	var eth struct {
		ForkID forkid.ID
		Tail   []rlp.RawValue `rlp:"tail"`
	}
	if n.Load(enr.WithEntry("eth", &eth)) != nil {
		return "", 0
	}
	return fmt.Sprintf("%x", eth.ForkID.Hash), eth.ForkID.Next
}

// dominantForkHash returns the fork hash (hex) with the highest aggregate node
// score across candidates.  Score-weighted selection ensures that high-quality
// nodes drive the selection rather than stale low-score nodes.
func dominantForkHash(candidates []candidateNode) string {
	type stats struct {
		count      int
		totalScore int
	}
	tally := make(map[string]*stats)
	for _, c := range candidates {
		if c.forkHash == "" {
			continue
		}
		s := tally[c.forkHash]
		if s == nil {
			s = &stats{}
			tally[c.forkHash] = s
		}
		s.count++
		s.totalScore += c.record.Score
	}
	// Primary: highest totalScore (favours the group of actively-responding,
	// high-quality nodes on the current fork).  Secondary tie-break: count.
	best := ""
	bestScore := -1
	bestCount := 0
	for fh, s := range tally {
		if s.totalScore > bestScore || (s.totalScore == bestScore && s.count > bestCount) {
			best = fh
			bestScore = s.totalScore
			bestCount = s.count
		}
	}
	return best
}

// ---------------------------------------------------------------------------
// Output helpers
// ---------------------------------------------------------------------------

func toOutputNode(c candidateNode) OutputNode {
	out := OutputNode{
		ENR:          c.record.Record,
		NodeID:       c.nodeID,
		Score:        c.record.Score,
		LastResponse: c.record.LastResponse,
		ForkID:       c.forkHash,
		ForkNext:     c.forkNext,
	}
	if ip := c.node.IP(); ip != nil {
		out.IP = ip.String()
	}
	if port := c.node.UDP(); port > 0 {
		out.Port = port
	}
	return out
}

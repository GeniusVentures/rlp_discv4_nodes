# rlp_discv4_nodes — EVM Bootstrap Node Filter

Automated system that downloads the latest
[`all.json`](https://github.com/ethereum/discv4-dns-lists) crawl from the
Ethereum discv4-dns-lists repository, filters nodes by EVM-compatible chain,
selects only those advertising the dominant (current-head) fork ID, ranks them
by score and recency, and writes clean JSON files of the top bootstrap peers
for use with your discv4 peer-discovery library.

## Output structure

```
output/
  ethereum-mainnet/latest-nodes.json
  ethereum-sepolia/latest-nodes.json
  ethereum-holesky/latest-nodes.json
  ethereum-hoodi/latest-nodes.json
  bnb-smart-chain/latest-nodes.json
  polygon-mainnet/latest-nodes.json   (if fork hashes configured)
  gnosis-chain/latest-nodes.json      (if fork hashes configured)
  ...
```

Each `latest-nodes.json` is an array of objects:

```json
[
  {
    "enr":          "enr:-...",
    "nodeId":       "04d70a61...",
    "score":        3371,
    "lastResponse": "2026-03-14T20:54:48Z",
    "forkId":       "4d518ce1",
    "forkNext":     0,
    "ip":           "162.55.86.114",
    "port":         30311
  },
  ...
]
```

Nodes are sorted by `score` (descending) then `lastResponse` (most recent
first), and capped at `topN` (default 100, configurable per chain).

## Automation

A [GitHub Actions workflow](.github/workflows/update.yml) runs every 6 hours,
rebuilds the binary, downloads the latest crawl, and commits any changed output
files back to the repository.

The workflow also triggers on pushes to `main` that change `chains_config.json`
or the Go source, and can be triggered manually via **Actions → Run workflow**.

## Local usage

```bash
# Build
go build -o filter_nodes .

# Run (downloads all.json from the internet)
./filter_nodes

# Use a local copy of all.json (faster for development)
./filter_nodes -input /path/to/all.json

# Custom config file
./filter_nodes -config chains_config.json

# Discovery mode: print ranked fork-hash summary to identify unknown chains
./filter_nodes -discover
```

## Supported filter strategies

| Strategy          | Description |
|-------------------|-------------|
| `geth_network`    | Uses `go-ethereum`'s `forkid.NewStaticFilter` to accept **any** node on the same genesis chain, regardless of current fork level.  Supported networks: `mainnet`, `sepolia`, `holesky`, `hoodi`. |
| `enr_field`       | Accepts nodes that carry a specific ENR key (e.g. `bsc` for BNB Smart Chain). |
| `fork_hash_list`  | Accepts nodes whose `eth` fork hash is in the configured `forkHashes` list.  Use `-discover` to identify the right hashes after each chain upgrade. |

## Adding or updating chains

1. Edit `chains_config.json`.
2. Run `./filter_nodes -discover` to print a ranked list of all fork hashes
   currently observed in the crawl alongside chain-specific ENR keys (e.g.
   `bsc(2709)` identifies BNB Smart Chain nodes).  Cross-reference the highest-
   count hashes with the chain's release notes to find the current fork hash.
3. Add the hash(es) to the chain's `forkHashes` array.
4. Re-run `./filter_nodes` to verify results.

### Known Ethereum fork hash progression (March 2026)

| Chain   | Fork     | Hash       | Timestamp / Block |
|---------|----------|------------|-------------------|
| Mainnet | Cancun   | `9f3d2254` | 1710338135        |
| Mainnet | Prague   | `c376cf8b` | 1746612311        |
| Mainnet | Osaka    | `5167e2a6` | 1764798551        |
| Mainnet | BPO1     | `cba2a1c0` | 1765290071        |
| Mainnet | **BPO2** | `07c9462e` | 1767747671 (current) |
| Sepolia | Cancun   | `88cf81d9` | 1706655072        |
| Sepolia | **BPO2** | `268956b6` | current           |
| Holesky | Cancun   | `9b192ad0` | 1707305664        |
| Holesky | **BPO2** | `9bc6cb31` | current           |
| Hoodi   | **BPO2** | `23aa1351` | current           |

## Dependencies

- [`github.com/ethereum/go-ethereum`](https://github.com/ethereum/go-ethereum) —
  for ENR decoding (`p2p/enode`, `p2p/enr`), fork ID filtering (`core/forkid`),
  and chain configurations (`params`).
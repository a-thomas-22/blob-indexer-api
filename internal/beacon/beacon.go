// Package beacon derives beacon-chain slot numbers for execution-layer
// blocks. Post-merge consensus enforces block.timestamp == genesis_time +
// slot * seconds_per_slot exactly (missed slots leave gaps in block numbers,
// never in the timestamp grid), so the slot of any blob-carrying block — all
// of which are post-Cancun, hence post-merge — is an exact function of its
// timestamp and the network's beacon genesis time.
package beacon

import "github.com/a-thomas-22/blob-indexer-api/internal/config"

// DefaultSecondsPerSlot is the beacon-chain SECONDS_PER_SLOT for every
// supported network. Networks with different slot timing must configure
// seconds_per_slot explicitly.
const DefaultSecondsPerSlot = 12

// knownGenesisTimes maps chain ID → the beacon chain's actual genesis time in
// unix seconds. These are protocol constants, immutable for the life of each
// network. Mainnet's differs from its MIN_GENESIS_TIME because genesis was
// triggered by the validator-count threshold; the testnet values are
// MIN_GENESIS_TIME + GENESIS_DELAY from each network's published consensus
// config (eth-clients metadata).
var knownGenesisTimes = map[int]uint64{
	1:        1606824023, // mainnet — verifies against the merge block: slot 4700013 ⇒ 1606824023 + 12*4700013 = 1663224179
	11155111: 1655733600, // sepolia — 1655647200 + 86400
	17000:    1695902400, // holesky — 1695902100 + 300
	560048:   1742213400, // hoodi — 1742212800 + 600
}

// Clock is a network's beacon-chain slot timing.
type Clock struct {
	GenesisTime    uint64
	SecondsPerSlot uint64
}

// ClockForNetwork resolves the beacon clock for a network: an explicit
// beacon_genesis_time in the network's configuration wins, else the compiled
// constant for known chain IDs. The second return is false when neither is
// available — slot derivation is impossible for such a network until its
// genesis time is configured.
func ClockForNetwork(n config.NetworkConfig) (Clock, bool) {
	genesis := n.BeaconGenesisTime
	if genesis == 0 {
		known, ok := knownGenesisTimes[n.ChainID]
		if !ok {
			return Clock{}, false
		}
		genesis = known
	}
	secondsPerSlot := n.SecondsPerSlot
	if secondsPerSlot == 0 {
		secondsPerSlot = DefaultSecondsPerSlot
	}
	return Clock{GenesisTime: genesis, SecondsPerSlot: secondsPerSlot}, true
}

// SlotAt returns the beacon slot whose slot time is the given block
// timestamp. The second return is false for timestamps before genesis, which
// no real blob-carrying block can have.
func (c Clock) SlotAt(timestamp uint64) (uint64, bool) {
	if timestamp < c.GenesisTime || c.SecondsPerSlot == 0 {
		return 0, false
	}
	return (timestamp - c.GenesisTime) / c.SecondsPerSlot, true
}

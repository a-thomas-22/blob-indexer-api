// Package beacon derives beacon-chain slot numbers for execution-layer
// blocks. Post-merge consensus enforces block.timestamp == genesis_time +
// slot * seconds_per_slot exactly (missed slots leave gaps in block numbers,
// never in the timestamp grid), so the slot of any blob-carrying block — all
// of which are post-Cancun, hence post-merge — is an exact function of its
// timestamp and the network's beacon genesis time.
package beacon

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
	5:        1616508000, // goerli/prater — 1614588812 + 1919188; long shut down, but the legacy RPC_URL sniffing can still synthesize chain 5
	11155111: 1655733600, // sepolia — 1655647200 + 86400
	17000:    1695902400, // holesky — 1695902100 + 300
	560048:   1742213400, // hoodi — 1742212800 + 600
}

// Clock is a network's beacon-chain slot timing.
type Clock struct {
	GenesisTime    uint64
	SecondsPerSlot uint64
}

// ResolveClock resolves the beacon clock for a network: an explicit
// genesisTime (a network's beacon_genesis_time configuration) wins, else the
// compiled constant for known chain IDs. secondsPerSlot zero means the
// 12-second default. The second return is false when no genesis time is
// available — slot derivation is impossible for such a network, which
// config validation treats as fatal for the indexer (blob-flow depends on
// the slot field) and the API treats as omission (it may still serve rows a
// past indexer wrote).
func ResolveClock(chainID int, genesisTime, secondsPerSlot uint64) (Clock, bool) {
	if genesisTime == 0 {
		known, ok := knownGenesisTimes[chainID]
		if !ok {
			return Clock{}, false
		}
		genesisTime = known
	}
	if secondsPerSlot == 0 {
		secondsPerSlot = DefaultSecondsPerSlot
	}
	return Clock{GenesisTime: genesisTime, SecondsPerSlot: secondsPerSlot}, true
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

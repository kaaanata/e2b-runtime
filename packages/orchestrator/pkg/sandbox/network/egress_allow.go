//go:build linux

package network

import (
	"encoding/binary"
	"fmt"

	"github.com/gaissmai/extnetip"
	"github.com/google/nftables"

	sandbox_network "github.com/e2b-dev/infra/packages/shared/pkg/sandbox-network"
)

const (
	// egressAllowSetName is the per-slot set holding the port-less entries of
	// the deployment egress allow-list (all protocols).
	egressAllowSetName = "filtered_egress_allowlist"

	// egressAllowPortSetName is the per-slot set holding the port-scoped entries
	// of the deployment egress allow-list, keyed by ip daddr . tcp dport.
	egressAllowPortSetName = "filtered_egress_allowlist_ports"

	// egressAllowPortRegister is the register `tcp dport` is loaded into for the
	// set lookup. nftables numbers the 32-bit registers from NFT_REG32_00 = 8;
	// the 128-bit NFT_REG_1 (= 1, where IPv4DestinationAddress puts the address)
	// aliases NFT_REG32_00..03, so NFT_REG32_01 = 9 is the word right after the
	// address and the lookup reads the concatenation ip . port from register 1.
	egressAllowPortRegister = 9

	// egressAllowPortKeyLen is the concatenated key size: ipv4_addr (4 bytes) +
	// inet_service (2 bytes, padded to the 4-byte register size).
	egressAllowPortKeyLen = 8
)

// egressAllowPortKeyType is the key type of egressAllowPortSetName.
func egressAllowPortKeyType() (nftables.SetDatatype, error) {
	return nftables.ConcatSetType(nftables.TypeIPAddr, nftables.TypeInetService)
}

// egressAllowPortKey encodes one ip . port element boundary: the IPv4 address
// in network order followed by the big-endian port and two bytes of padding.
func egressAllowPortKey(ip [4]byte, port uint16) []byte {
	key := make([]byte, egressAllowPortKeyLen)
	copy(key, ip[:])
	binary.BigEndian.PutUint16(key[4:], port)

	return key
}

// egressAllowPortSetElements renders the port-scoped allow-list entries as
// interval elements of a concatenated ipv4_addr . inet_service set: each entry
// is the range [first address . port, last address . port]. A /32 entry is a
// one-address range. The parser already rejects overlapping entries, which the
// kernel would refuse within one transaction.
func egressAllowPortSetElements(entries []sandbox_network.EgressAllowEntry) ([]nftables.SetElement, error) {
	elements := make([]nftables.SetElement, 0, len(entries))
	for _, e := range entries {
		if !e.Prefix.Addr().Is4() {
			return nil, fmt.Errorf("egress allow entry %s: only IPv4 can be encoded into the port set", e)
		}
		if e.Port == 0 {
			return nil, fmt.Errorf("egress allow entry %s: port-less entries belong in the address allow set", e)
		}

		first, last := extnetip.Range(e.Prefix)
		elements = append(elements, nftables.SetElement{
			Key:    egressAllowPortKey(first.As4(), e.Port),
			KeyEnd: egressAllowPortKey(last.As4(), e.Port),
		})
	}

	return elements, nil
}

//go:build linux

package network

import (
	"net/netip"
	"os"
	"runtime"
	"slices"
	"testing"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netns"

	sandbox_network "github.com/e2b-dev/infra/packages/shared/pkg/sandbox-network"
)

// TestParseConfig_SandboxEgressAllow pins the startup contract of
// SANDBOX_EGRESS_ALLOW: a valid value is parsed into the typed allow-list, an
// invalid one refuses to start, and an unset/empty one is an empty list.
func TestParseConfig_SandboxEgressAllow(t *testing.T) { //nolint:paralleltest // t.Setenv
	t.Run("valid", func(t *testing.T) { //nolint:paralleltest // t.Setenv
		t.Setenv("SANDBOX_EGRESS_ALLOW", "192.168.0.232:18781, 10.1.0.0/16")

		cfg, err := ParseConfig()
		require.NoError(t, err)
		assert.Equal(t, "192.168.0.232:18781,10.1.0.0/16", cfg.SandboxEgressAllow.String())
		assert.Equal(t, []string{"10.1.0.0/16"}, cfg.SandboxEgressAllow.PortlessCIDRs())
		require.Len(t, cfg.SandboxEgressAllow.PortScoped(), 1)
	})

	t.Run("unset is empty", func(t *testing.T) { //nolint:paralleltest // t.Setenv
		t.Setenv("SANDBOX_EGRESS_ALLOW", "")

		cfg, err := ParseConfig()
		require.NoError(t, err)
		assert.True(t, cfg.SandboxEgressAllow.IsEmpty())
	})

	for _, invalid := range []string{"mirror.lan:18781", "192.168.0.232:0", "fe80::1", "0.0.0.0/0", "10.0.0.0/8,10.1.0.0/16"} {
		t.Run("invalid "+invalid+" fails fast", func(t *testing.T) { //nolint:paralleltest // t.Setenv
			t.Setenv("SANDBOX_EGRESS_ALLOW", invalid)

			_, err := ParseConfig()
			require.ErrorContains(t, err, "SandboxEgressAllow", "the env parser names the field")
		})
	}
}

// TestEgressAllowPortSetElements pins the wire encoding of the concatenated
// ipv4_addr . inet_service interval elements: address in network order, port
// big-endian, padded to the 4-byte register, [first . port, last . port].
func TestEgressAllowPortSetElements(t *testing.T) {
	t.Parallel()

	list, err := sandbox_network.ParseEgressAllowList("192.168.0.232:18781,10.1.2.0/24:443,10.0.0.0/8")
	require.NoError(t, err)

	elements, err := egressAllowPortSetElements(list.PortScoped())
	require.NoError(t, err)
	require.Len(t, elements, 2, "only the port-scoped entries are encoded")

	// 18781 = 0x495d, 443 = 0x01bb
	assert.Equal(t, []byte{192, 168, 0, 232, 0x49, 0x5d, 0, 0}, elements[0].Key)
	assert.Equal(t, []byte{192, 168, 0, 232, 0x49, 0x5d, 0, 0}, elements[0].KeyEnd, "a /32 is a one-address range")
	assert.Equal(t, []byte{10, 1, 2, 0, 0x01, 0xbb, 0, 0}, elements[1].Key)
	assert.Equal(t, []byte{10, 1, 2, 255, 0x01, 0xbb, 0, 0}, elements[1].KeyEnd)
	for _, e := range elements {
		assert.False(t, e.IntervalEnd, "concatenated ranges use KeyEnd, not INTERVAL_END boundaries")
		assert.Len(t, e.Key, egressAllowPortKeyLen)
	}

	keyType, err := egressAllowPortKeyType()
	require.NoError(t, err)
	assert.Equal(t, uint32(egressAllowPortKeyLen), keyType.Bytes, "set key length must match the element encoding")

	_, err = egressAllowPortSetElements(list.Entries())
	require.ErrorContains(t, err, "port-less", "port-less entries must never land in the port set")
}

// TestNewFirewall_DeploymentEgressAllow builds a real slot netns + nftables
// ruleset with SANDBOX_EGRESS_ALLOW set and checks: the port-scoped entries land
// in the concatenated set as [ip . port] with a TCP-only accept rule; the
// port-less entry lands in its own set (NOT the predefined allow set) behind
// the non-TCP userAllow-accept / userDeny-drop pair and an all-protocol accept;
// all of them sit AFTER the predefined allow accept and BEFORE the predefined
// deny drop, so the deployment list beats the private-range deny set while a
// sandbox's denyOut still wins; and ApplyRules (user rules, BYOP, reset) keeps
// them. It needs root; it skips otherwise — same gating as the other
// privileged tests in this package.
func TestNewFirewall_DeploymentEgressAllow(t *testing.T) { //nolint:paralleltest // mutates the caller's netns via LockOSThread + netns.Set; cannot run in parallel
	if os.Geteuid() != 0 {
		t.Skip("requires root for netns + nftables")
	}

	t.Setenv("SANDBOX_EGRESS_ALLOW", "192.168.0.232:18781,10.1.2.0/24:443,100.127.50.0/24")

	config, err := ParseConfig()
	require.NoError(t, err)

	slot, err := NewSlot("egress-allow-test", reserveNSTestIdx(t), config, NewNoopEgressProxy())
	require.NoError(t, err)

	require.NoError(t, slot.CreateNetwork(t.Context()))
	t.Cleanup(func() { _ = slot.RemoveNetwork() })

	check := func(t *testing.T) {
		t.Helper()

		got := inspectEgressAllow(t, slot)

		require.True(t, got.portRuleFound, "the TCP ip . port accept rule must be installed")
		require.GreaterOrEqual(t, got.predefinedAllowIdx, 0)
		require.GreaterOrEqual(t, got.predefinedDenyIdx, 0)
		assert.Less(t, got.predefinedAllowIdx, got.portRuleIdx, "the port rule comes after the predefined allow accept")
		assert.Less(t, got.portRuleIdx, got.predefinedDenyIdx, "the port rule comes before the predefined deny drop")

		// Port-less entry: userAllow accept, then userDeny drop, then accept —
		// all ahead of the predefined deny drop.
		require.GreaterOrEqual(t, got.portlessUserAllowIdx, 0, "non-TCP egress ∩ userAllow accept rule")
		require.GreaterOrEqual(t, got.portlessUserDenyIdx, 0, "non-TCP egress ∩ userDeny drop rule")
		require.GreaterOrEqual(t, got.portlessAcceptIdx, 0, "egress allow accept rule")
		assert.Less(t, got.portlessUserAllowIdx, got.portlessUserDenyIdx, "allowOut beats denyOut, as in rules 4/5")
		assert.Less(t, got.portlessUserDenyIdx, got.portlessAcceptIdx, "a sandbox denyOut wins over the deployment allow")
		assert.Less(t, got.portlessAcceptIdx, got.predefinedDenyIdx, "the deployment allow beats the predefined deny set")

		assertIPv4SetMembership(t, got.egressAllowElements, []ipv4SetProbe{
			{netip.MustParseAddr("100.127.50.0"), true},
			{netip.MustParseAddr("100.127.50.99"), true},
			{netip.MustParseAddr("100.127.50.255"), true},
			{netip.MustParseAddr("100.127.51.1"), false},
			{netip.MustParseAddr("192.168.0.232"), false}, // port-scoped: TCP-only set
		})

		require.Len(t, got.portElements, 2)
		assert.Contains(t, got.portElements, egressAllowPortElement{
			start: netip.MustParseAddr("192.168.0.232"), end: netip.MustParseAddr("192.168.0.232"), port: 18781,
		})
		assert.Contains(t, got.portElements, egressAllowPortElement{
			start: netip.MustParseAddr("10.1.2.0"), end: netip.MustParseAddr("10.1.2.255"), port: 443,
		})

		assertIPv4SetMembership(t, got.predefinedAllowElements, []ipv4SetProbe{
			{netip.MustParseAddr(config.OrchestratorInSandboxIPAddress), true},
			{netip.MustParseAddr("100.127.50.99"), false}, // deployment entries have their own set
			{netip.MustParseAddr("192.168.0.232"), false},
		})
	}

	check(t)

	// The deployment entries survive every rebuild of the chain: user rules,
	// BYOP mode (TCP shifts to the SOCKS proxy) and the reset for the next tenant.
	require.NoError(t, slot.Firewall.ApplyRules(t.Context(), false, []string{"1.1.1.1"}, []string{"0.0.0.0/0"}))
	check(t)
	require.NoError(t, slot.Firewall.ApplyRules(t.Context(), true, nil, nil))
	check(t)
	require.NoError(t, slot.Firewall.ApplyRules(t.Context(), false, nil, nil))
	check(t)
}

// TestNewFirewall_NoEgressAllowHasNoPortSet verifies a node without the feature
// gets exactly the pre-existing ruleset: no concatenated set, no port rule.
func TestNewFirewall_NoEgressAllowHasNoPortSet(t *testing.T) { //nolint:paralleltest // mutates the caller's netns via LockOSThread + netns.Set; cannot run in parallel
	if os.Geteuid() != 0 {
		t.Skip("requires root for netns + nftables")
	}

	t.Setenv("SANDBOX_EGRESS_ALLOW", "")

	config, err := ParseConfig()
	require.NoError(t, err)

	slot, err := NewSlot("egress-allow-none-test", reserveNSTestIdx(t), config, NewNoopEgressProxy())
	require.NoError(t, err)

	require.NoError(t, slot.CreateNetwork(t.Context()))
	t.Cleanup(func() { _ = slot.RemoveNetwork() })

	got := inspectEgressAllow(t, slot)
	assert.False(t, got.portRuleFound)
	assert.False(t, got.portSetFound)
	assert.False(t, got.egressSetFound)
	assert.Equal(t, -1, got.portlessAcceptIdx)
	assert.Nil(t, slot.Firewall.egressAllowPortSet)
	assert.Nil(t, slot.Firewall.egressAllowSet)
}

type egressAllowPortElement struct {
	start, end netip.Addr
	port       uint16
}

type egressAllowInspection struct {
	portSetFound   bool
	egressSetFound bool
	portRuleFound  bool
	// Rule indexes in the filter chain (-1 = absent).
	predefinedAllowIdx, portRuleIdx, predefinedDenyIdx           int
	portlessUserAllowIdx, portlessUserDenyIdx, portlessAcceptIdx int

	portElements            []egressAllowPortElement
	egressAllowElements     []nftables.SetElement
	predefinedAllowElements []nftables.SetElement
}

// inspectEgressAllow enters the slot's netns and reads back the sets and the
// filter chain as the kernel holds them.
func inspectEgressAllow(t *testing.T, slot *Slot) egressAllowInspection {
	t.Helper()

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	host, err := netns.Get()
	require.NoError(t, err)
	defer func() {
		require.NoError(t, netns.Set(host))
		_ = host.Close()
	}()

	target, err := netns.GetFromName(slot.NamespaceID())
	require.NoError(t, err)
	defer func() { _ = target.Close() }()
	require.NoError(t, netns.Set(target))

	conn, err := nftables.New()
	require.NoError(t, err)

	table := &nftables.Table{Name: tableName, Family: nftables.TableFamilyINet}

	var got egressAllowInspection
	got.predefinedAllowIdx, got.portRuleIdx, got.predefinedDenyIdx = -1, -1, -1
	got.portlessUserAllowIdx, got.portlessUserDenyIdx, got.portlessAcceptIdx = -1, -1, -1

	sets, err := conn.GetSets(table)
	require.NoError(t, err)
	for _, s := range sets {
		switch s.Name {
		case egressAllowPortSetName:
			got.portSetFound = true
			require.True(t, s.Concatenation, "port set must be a concatenation")
			require.True(t, s.Interval, "port set must be an interval set")

			elements, err := conn.GetSetElements(s)
			require.NoError(t, err)
			for _, e := range elements {
				require.Len(t, e.Key, egressAllowPortKeyLen)
				require.Len(t, e.KeyEnd, egressAllowPortKeyLen)
				start, _ := netip.AddrFromSlice(e.Key[:4])
				end, _ := netip.AddrFromSlice(e.KeyEnd[:4])
				require.Equal(t, e.Key[4:], e.KeyEnd[4:], "a range spans addresses, not ports")
				got.portElements = append(got.portElements, egressAllowPortElement{
					start: start, end: end, port: uint16(e.Key[4])<<8 | uint16(e.Key[5]),
				})
			}
		case egressAllowSetName:
			got.egressSetFound = true
			got.egressAllowElements, err = conn.GetSetElements(s)
			require.NoError(t, err)
		case "filtered_always_allowlist":
			got.predefinedAllowElements, err = conn.GetSetElements(s)
			require.NoError(t, err)
		}
	}

	chain := &nftables.Chain{Name: "PREROUTE_FILTER", Table: table}
	rules, err := conn.GetRules(table, chain)
	require.NoError(t, err)
	for i, r := range rules {
		var drop, accept, l4proto bool
		var lookups []string
		for _, e := range r.Exprs {
			switch x := e.(type) {
			case *expr.Meta:
				if x.Key == expr.MetaKeyL4PROTO {
					l4proto = true
				}
			case *expr.Verdict:
				switch x.Kind {
				case expr.VerdictDrop:
					drop = true
				case expr.VerdictAccept:
					accept = true
				}
			case *expr.Lookup:
				lookups = append(lookups, x.SetName)
			}
		}
		has := func(name string) bool { return slices.Contains(lookups, name) }
		switch {
		case has(egressAllowPortSetName) && accept:
			require.True(t, l4proto, "the port rule must be TCP-only")
			got.portRuleFound = true
			got.portRuleIdx = i
		case has(egressAllowSetName) && has("filtered_allowlist") && accept:
			require.True(t, l4proto, "the userAllow pair rule must be non-TCP only")
			got.portlessUserAllowIdx = i
		case has(egressAllowSetName) && has("filtered_denylist") && drop:
			require.True(t, l4proto, "the userDeny pair rule must be non-TCP only")
			got.portlessUserDenyIdx = i
		case has(egressAllowSetName) && accept && len(lookups) == 1:
			require.False(t, l4proto, "the port-less accept covers every protocol")
			got.portlessAcceptIdx = i
		case has("filtered_always_allowlist") && accept && !l4proto:
			got.predefinedAllowIdx = i
		case has("filtered_always_denylist") && drop && got.predefinedDenyIdx == -1:
			got.predefinedDenyIdx = i
		}
	}

	return got
}

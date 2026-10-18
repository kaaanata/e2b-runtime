package sandbox_network

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseEgressAllowEntry(t *testing.T) {
	t.Parallel()

	valid := []struct {
		input      string
		wantPrefix string
		wantPort   uint16
		wantString string
	}{
		{"192.168.0.232:18781", "192.168.0.232/32", 18781, "192.168.0.232:18781"},
		{"192.168.0.232", "192.168.0.232/32", 0, "192.168.0.232"},
		{"10.1.0.0/16", "10.1.0.0/16", 0, "10.1.0.0/16"},
		{"10.1.2.0/24:443", "10.1.2.0/24", 443, "10.1.2.0/24:443"},
		{"  100.127.50.99:22 ", "100.127.50.99/32", 22, "100.127.50.99:22"},
		// Host bits are masked off, like nft does.
		{"10.1.2.3/24:80", "10.1.2.0/24", 80, "10.1.2.0/24:80"},
		{"1.2.3.4/32", "1.2.3.4/32", 0, "1.2.3.4"},
	}
	for _, tc := range valid {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()

			got, err := ParseEgressAllowEntry(tc.input)
			require.NoError(t, err)
			assert.Equal(t, netip.MustParsePrefix(tc.wantPrefix), got.Prefix)
			assert.Equal(t, tc.wantPort, got.Port)
			assert.Equal(t, tc.wantString, got.String())
		})
	}

	invalid := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", "must not be empty"},
		{"hostname", "mirror.lan:18781", "unexpected character"},
		{"ipv6", "fe80::1", "only IPv4"},
		{"ipv6 with port", "[fe80::1]:80", "only IPv4"},
		{"ipv6 prefix", "fc00::/7", "only IPv4"},
		{"port zero", "192.168.0.232:0", "not a valid 1-65535"},
		{"port too large", "192.168.0.232:65536", "not a valid 1-65535"},
		{"port not a number", "192.168.0.232:http", "not a valid 1-65535"},
		{"empty port", "192.168.0.232:", "not a valid 1-65535"},
		{"unspecified address", "0.0.0.0:80", "must be specified"},
		{"unspecified prefix", "0.0.0.0/8", "must be specified"},
		{"catch-all prefix", "0.0.0.0/0", "must be specified"},
		{"catch-all prefix non-zero address", "1.0.0.0/0:80", "/0 prefix would allow all traffic"},
		{"bad prefix length", "10.0.0.0/33", "prefix length out of range"},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := ParseEgressAllowEntry(tc.input)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestParseEgressAllowList(t *testing.T) {
	t.Parallel()

	t.Run("empty and whitespace-only are empty lists", func(t *testing.T) {
		t.Parallel()

		for _, raw := range []string{"", "  ", ",", " , ,"} {
			list, err := ParseEgressAllowList(raw)
			require.NoError(t, err, "%q", raw)
			assert.True(t, list.IsEmpty())
			assert.Empty(t, list.PortlessCIDRs())
			assert.Empty(t, list.PortScoped())
			assert.Empty(t, list.String())
		}
	})

	t.Run("splits port-scoped and port-less entries", func(t *testing.T) {
		t.Parallel()

		list, err := ParseEgressAllowList("192.168.0.232:18781, 10.1.0.0/16 ,100.127.50.99,")
		require.NoError(t, err)
		require.False(t, list.IsEmpty())
		assert.Equal(t, []string{"10.1.0.0/16", "100.127.50.99/32"}, list.PortlessCIDRs())
		require.Len(t, list.PortScoped(), 1)
		assert.Equal(t, "192.168.0.232:18781", list.PortScoped()[0].String())
		assert.Equal(t, "192.168.0.232:18781,10.1.0.0/16,100.127.50.99", list.String())
		assert.Len(t, list.Entries(), 3)
	})

	t.Run("round-trips through String", func(t *testing.T) {
		t.Parallel()

		list, err := ParseEgressAllowList("192.168.0.232:18781,10.1.2.0/24:443,10.2.0.0/16")
		require.NoError(t, err)
		again, err := ParseEgressAllowList(list.String())
		require.NoError(t, err)
		assert.Equal(t, list, again)
	})

	t.Run("one bad entry fails the whole list", func(t *testing.T) {
		t.Parallel()

		_, err := ParseEgressAllowList("192.168.0.232:18781,mirror.lan:80")
		require.ErrorContains(t, err, `"mirror.lan:80"`)
	})

	t.Run("duplicate and overlapping entries with the same port scope are rejected", func(t *testing.T) {
		t.Parallel()

		for _, raw := range []string{
			"192.168.0.232:18781,192.168.0.232:18781",
			"192.168.0.0/24:18781,192.168.0.232:18781",
			"10.0.0.0/8,10.1.0.0/16",
			"10.1.2.3,10.1.2.3/32",
		} {
			_, err := ParseEgressAllowList(raw)
			require.ErrorContains(t, err, "overlap", "%q", raw)
		}
	})

	t.Run("same prefix with different ports is fine", func(t *testing.T) {
		t.Parallel()

		list, err := ParseEgressAllowList("192.168.0.232:80,192.168.0.232:443,192.168.0.232")
		require.NoError(t, err)
		assert.Len(t, list.PortScoped(), 2)
		assert.Equal(t, []string{"192.168.0.232/32"}, list.PortlessCIDRs())
	})
}

func TestEgressAllowList_UnmarshalText(t *testing.T) {
	t.Parallel()

	var list EgressAllowList
	require.NoError(t, list.UnmarshalText([]byte("192.168.0.232:18781")))
	assert.Equal(t, "192.168.0.232:18781", list.String())

	require.NoError(t, list.UnmarshalText(nil))
	assert.True(t, list.IsEmpty(), "an empty value resets the list")

	require.Error(t, list.UnmarshalText([]byte("not-an-ip")))
}

func TestEgressAllowList_Allows(t *testing.T) {
	t.Parallel()

	list, err := ParseEgressAllowList("192.168.0.232:18781,10.1.0.0/16,172.16.5.0/24:443")
	require.NoError(t, err)

	cases := []struct {
		ip   string
		port uint16
		want bool
	}{
		{"192.168.0.232", 18781, true},
		{"192.168.0.232", 18782, false},
		{"192.168.0.232", 80, false},
		{"192.168.0.233", 18781, false},
		{"10.1.2.3", 1, true},
		{"10.1.255.255", 65535, true},
		{"10.2.0.0", 80, false},
		{"172.16.5.9", 443, true},
		{"172.16.5.9", 80, false},
		{"172.16.6.1", 443, false},
		// IPv4-mapped IPv6 (what net.IP often carries) is unmapped before matching.
		{"::ffff:192.168.0.232", 18781, true},
		{"fe80::1", 18781, false},
	}
	for _, tc := range cases {
		assert.Equalf(t, tc.want, list.Allows(netip.MustParseAddr(tc.ip), tc.port), "%s:%d", tc.ip, tc.port)
	}

	assert.False(t, EgressAllowList{}.Allows(netip.MustParseAddr("192.168.0.232"), 18781), "empty list allows nothing")
}

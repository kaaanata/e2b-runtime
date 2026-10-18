package sandbox_network

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// EgressAllowEntry is one destination an operator opens for every sandbox and
// template-build VM on a node, ahead of DeniedSandboxCIDRs. Port 0 means every
// port and every protocol; a non-zero port scopes the entry to TCP traffic to
// that port only.
type EgressAllowEntry struct {
	Prefix netip.Prefix
	Port   uint16
}

// String renders the entry in the IP[/prefix][:port] grammar ParseEgressAllowEntry
// accepts; a /32 prefix is rendered as a bare address.
func (e EgressAllowEntry) String() string {
	s := e.Prefix.String()
	if e.Prefix.IsSingleIP() {
		s = e.Prefix.Addr().String()
	}
	if e.Port != 0 {
		s += ":" + strconv.Itoa(int(e.Port))
	}

	return s
}

// Matches reports whether the entry covers a connection to ip:port. A
// port-less entry covers every port; a port-scoped entry only its own.
func (e EgressAllowEntry) Matches(ip netip.Addr, port uint16) bool {
	if !e.Prefix.Contains(ip.Unmap()) {
		return false
	}

	return e.Port == 0 || e.Port == port
}

// EgressAllowList is a validated deployment-level egress allow-list, parsed from
// the comma-separated SANDBOX_EGRESS_ALLOW environment variable. It implements
// encoding.TextUnmarshaler so env parsing fails fast on an invalid value.
//
// The allow-list is an operator decision for the whole node: its destinations are
// accepted before the predefined private-range deny set (DeniedSandboxCIDRs) for
// every sandbox and template build. A sandbox's own allowOut/denyOut still apply
// on top of it, so an explicit denyOut can block an allowed destination.
type EgressAllowList struct { //nolint:recvcheck // encoding.TextUnmarshaler needs a pointer receiver; everything else is by value
	entries []EgressAllowEntry
}

// ParseEgressAllowList parses a comma-separated list of IP[/prefix][:port]
// entries (IPv4 only). Empty items are skipped; duplicate or overlapping entries
// (same port scope, overlapping prefixes) are rejected because the kernel sets
// they feed do not merge overlapping intervals.
func ParseEgressAllowList(raw string) (EgressAllowList, error) {
	var list EgressAllowList
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		entry, err := ParseEgressAllowEntry(part)
		if err != nil {
			return EgressAllowList{}, err
		}

		for _, existing := range list.entries {
			if existing.Port == entry.Port && existing.Prefix.Overlaps(entry.Prefix) {
				return EgressAllowList{}, fmt.Errorf("egress allow entries %q and %q overlap", existing, entry)
			}
		}

		list.entries = append(list.entries, entry)
	}

	return list, nil
}

// ParseEgressAllowEntry parses one IP[/prefix][:port] entry, e.g. "192.168.0.232:18781",
// "10.1.0.0/16" or "10.1.2.0/24:443". Only IPv4 is supported (the kernel allow
// sets are IPv4), the address must be specified (not 0.0.0.0) and the prefix
// must not be /0: the list widens the predefined deny set, so a catch-all entry
// is almost certainly a mistake.
func ParseEgressAllowEntry(s string) (EgressAllowEntry, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return EgressAllowEntry{}, errors.New("egress allow entry must not be empty")
	}
	if strings.Count(s, ":") > 1 {
		return EgressAllowEntry{}, fmt.Errorf("egress allow entry %q: only IPv4 addresses are supported", s)
	}

	hostPart, portPart, hasPort := strings.Cut(s, ":")

	var port uint16
	if hasPort {
		p, err := strconv.ParseUint(portPart, 10, 16)
		if err != nil || p == 0 {
			return EgressAllowEntry{}, fmt.Errorf("egress allow entry %q: port %q is not a valid 1-65535 value", s, portPart)
		}
		port = uint16(p)
	}

	var prefix netip.Prefix
	if strings.Contains(hostPart, "/") {
		p, err := netip.ParsePrefix(hostPart)
		if err != nil {
			return EgressAllowEntry{}, fmt.Errorf("egress allow entry %q: %w", s, err)
		}
		prefix = p
	} else {
		addr, err := netip.ParseAddr(hostPart)
		if err != nil {
			return EgressAllowEntry{}, fmt.Errorf("egress allow entry %q: %w", s, err)
		}
		prefix = netip.PrefixFrom(addr, addr.BitLen())
	}

	if !prefix.Addr().Is4() {
		return EgressAllowEntry{}, fmt.Errorf("egress allow entry %q: only IPv4 addresses are supported", s)
	}
	if prefix.Addr().IsUnspecified() {
		return EgressAllowEntry{}, fmt.Errorf("egress allow entry %q: address must be specified (not 0.0.0.0)", s)
	}
	if prefix.Bits() == 0 {
		return EgressAllowEntry{}, fmt.Errorf("egress allow entry %q: a /0 prefix would allow all traffic; list the ranges explicitly", s)
	}

	return EgressAllowEntry{Prefix: prefix.Masked(), Port: port}, nil
}

// UnmarshalText implements encoding.TextUnmarshaler for env parsing.
func (l *EgressAllowList) UnmarshalText(text []byte) error {
	parsed, err := ParseEgressAllowList(string(text))
	if err != nil {
		return err
	}
	*l = parsed

	return nil
}

// String renders the list in the comma-separated grammar it was parsed from.
func (l EgressAllowList) String() string {
	parts := make([]string, 0, len(l.entries))
	for _, e := range l.entries {
		parts = append(parts, e.String())
	}

	return strings.Join(parts, ",")
}

// Entries returns every entry in input order.
func (l EgressAllowList) Entries() []EgressAllowEntry {
	return append([]EgressAllowEntry(nil), l.entries...)
}

// IsEmpty reports whether the list has no entries.
func (l EgressAllowList) IsEmpty() bool {
	return len(l.entries) == 0
}

// PortlessCIDRs returns the entries without a port as CIDR strings: they are
// allowed for every protocol and port.
func (l EgressAllowList) PortlessCIDRs() []string {
	var out []string
	for _, e := range l.entries {
		if e.Port == 0 {
			out = append(out, e.Prefix.String())
		}
	}

	return out
}

// PortScoped returns the entries with a port: they are allowed for TCP to that
// port only.
func (l EgressAllowList) PortScoped() []EgressAllowEntry {
	var out []EgressAllowEntry
	for _, e := range l.entries {
		if e.Port != 0 {
			out = append(out, e)
		}
	}

	return out
}

// Allows reports whether a connection to ip:port is covered by the list.
func (l EgressAllowList) Allows(ip netip.Addr, port uint16) bool {
	for _, e := range l.entries {
		if e.Matches(ip, port) {
			return true
		}
	}

	return false
}

package main

import "testing"

// Task 1 tests. The darwin and linux subtests cover the pure parsers that
// collectGatewayMAC and collectLocalSubnet call through run/ReadFile wrappers.
// A canned-output test on the parsers is the honest unit: wrapping exec or
// /proc under injectable runners for the one-liner wrappers would add more
// scaffolding than it covers.

func TestCollectGatewayMAC_ReturnsLowercaseColonForm(t *testing.T) {
	t.Run("darwin arp fixture", func(t *testing.T) {
		out := "? (192.168.1.1) at A4:83:E7:AA:BB:CC on en0 ifscope [ethernet]\n"
		got := parseMACFromDarwinArp(out, "192.168.1.1")
		want := "a4:83:e7:aa:bb:cc"
		if got != want {
			t.Fatalf("want %q, got %q", want, got)
		}
	})
	t.Run("linux /proc/net/arp fixture", func(t *testing.T) {
		contents := "IP address       HW type     Flags       HW address            Mask     Device\n" +
			"192.168.1.1      0x1         0x2         AA:BB:CC:DD:EE:FF     *        wlan0\n"
		got := parseMACFromLinuxProcARP(contents, "192.168.1.1")
		want := "aa:bb:cc:dd:ee:ff"
		if got != want {
			t.Fatalf("want %q, got %q", want, got)
		}
	})
}

func TestCollectGatewayMAC_EmptyWhenNoDefaultRoute(t *testing.T) {
	t.Run("darwin route output has no gateway line", func(t *testing.T) {
		out := "   route to: default\n   destination: default\n   mask: 0.0.0.0\n"
		if got := parseGatewayIPFromDarwinRoute(out); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})
	t.Run("linux /proc/net/route has no default row", func(t *testing.T) {
		contents := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n" +
			"wlan0\t0101A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n"
		gwIP, _ := parseGatewayFromLinuxProcRoute(contents)
		if gwIP != "" {
			t.Fatalf("want empty gwIP, got %q", gwIP)
		}
	})
}

func TestCollectLocalSubnet_ReturnsCIDR(t *testing.T) {
	t.Run("darwin ifconfig /24 netmask", func(t *testing.T) {
		out := "en0: flags=8863<UP,BROADCAST,SMART,RUNNING,SIMPLEX,MULTICAST> mtu 1500\n" +
			"\tinet 192.168.1.42 netmask 0xffffff00 broadcast 192.168.1.255\n"
		got := parseLocalSubnetFromDarwinIfconfig(out, "en0")
		want := "192.168.1.0/24"
		if got != want {
			t.Fatalf("want %q, got %q", want, got)
		}
	})
	t.Run("linux /proc/net/route /24 mask", func(t *testing.T) {
		// dest 0001A8C0 LE = 192.168.1.0; gw 00000000; mask 00FFFFFF LE = 255.255.255.0 (/24)
		contents := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n" +
			"wlan0\t0001A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n"
		got := parseLocalSubnetFromLinuxProcRoute(contents, "wlan0")
		want := "192.168.1.0/24"
		if got != want {
			t.Fatalf("want %q, got %q", want, got)
		}
	})
}

func TestCollectLocalSubnet_EmptyWhenNoMask(t *testing.T) {
	t.Run("darwin ifconfig missing netmask", func(t *testing.T) {
		out := "en0: flags=...\n\tinet 192.168.1.42\n"
		if got := parseLocalSubnetFromDarwinIfconfig(out, "en0"); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})
	t.Run("linux /proc/net/route has no row for iface", func(t *testing.T) {
		contents := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n"
		if got := parseLocalSubnetFromLinuxProcRoute(contents, "wlan0"); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})
}

func TestNormalizeMAC(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"AA:BB:CC:DD:EE:FF", "aa:bb:cc:dd:ee:ff"},
		{"aa-bb-cc-dd-ee-ff", "aa:bb:cc:dd:ee:ff"},
		{"a4:83:e7:aa:bb:cc", "a4:83:e7:aa:bb:cc"},
		{"notamac", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeMAC(c.in); got != c.want {
			t.Fatalf("normalizeMAC(%q): want %q, got %q", c.in, c.want, got)
		}
	}
}

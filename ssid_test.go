package main

import "testing"

// Task 2 tests. Covers the parsers extracted from netStatus() so SSID
// extraction can be exercised from canned fixtures without exec.

func TestParseSSIDFromWdutil(t *testing.T) {
	t.Run("returns SSID from wdutil info", func(t *testing.T) {
		out := "    WIFI\n" +
			"    ----\n" +
			"    MAC Address        : aa:bb:cc:dd:ee:ff\n" +
			"    Interface Name     : en0\n" +
			"    Power              : On [On]\n" +
			"    Op Mode            : STA\n" +
			"    SSID               : Familia Hernandez 5G\n" +
			"    BSSID              : 11:22:33:44:55:66\n"
		if got, want := parseSSIDFromWdutil(out), "Familia Hernandez 5G"; got != want {
			t.Fatalf("want %q, got %q", want, got)
		}
	})
	t.Run("returns empty when SSID is 'none'", func(t *testing.T) {
		out := "    SSID               : none\n"
		if got := parseSSIDFromWdutil(out); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})
	t.Run("returns empty on empty output", func(t *testing.T) {
		if got := parseSSIDFromWdutil(""); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})
}

func TestParseSSIDFromIpconfigSummary(t *testing.T) {
	t.Run("returns SSID from ipconfig getsummary", func(t *testing.T) {
		// ipconfig getsummary prints a key/value tree; SSID appears as an
		// indented line.
		out := "  IPv4 : <dictionary>\n" +
			"  SSID : STEVEN.SIERRA_5G\n" +
			"  Security : WPA2 Personal\n"
		if got, want := parseSSIDFromIpconfigSummary(out), "STEVEN.SIERRA_5G"; got != want {
			t.Fatalf("want %q, got %q", want, got)
		}
	})
	t.Run("returns empty on redacted", func(t *testing.T) {
		out := "  SSID : <redacted>\n"
		if got := parseSSIDFromIpconfigSummary(out); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})
	t.Run("returns empty when no SSID line", func(t *testing.T) {
		out := "  IPv4 : <dictionary>\n"
		if got := parseSSIDFromIpconfigSummary(out); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})
}

func TestParseSSIDFromIwLink(t *testing.T) {
	t.Run("returns SSID from iw dev link", func(t *testing.T) {
		out := "Connected to aa:bb:cc:dd:ee:ff (on wlan0)\n" +
			"\tSSID: casa-sierra\n" +
			"\tfreq: 5180\n" +
			"\tRX: 123456 bytes (42 packets)\n"
		if got, want := parseSSIDFromIwLink(out), "casa-sierra"; got != want {
			t.Fatalf("want %q, got %q", want, got)
		}
	})
	t.Run("returns empty when no SSID line", func(t *testing.T) {
		out := "Not connected.\n"
		if got := parseSSIDFromIwLink(out); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})
	t.Run("returns empty on empty output", func(t *testing.T) {
		if got := parseSSIDFromIwLink(""); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})
}

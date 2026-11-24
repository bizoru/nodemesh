package main

import "strings"

// Parsers for the three OS-specific SSID channels netStatus walks. Each
// takes the tool's full stdout and returns the SSID, or "" when the tool
// doesn't reveal one. Extracted from the inline logic in netStatus() so
// they can be unit-tested from canned fixtures.
//
// `ssidRe` (declared in main.go) matches the shape most of these tools
// share: an "SSID" label, optional colon, value to end-of-line.

// parseSSIDFromWdutil reads `wdutil info` output. Returns "" when the
// line is missing or the value is literally "none" — wdutil prints that
// when wifi is on but not associated.
func parseSSIDFromWdutil(out string) string {
	m := ssidRe.FindStringSubmatch(out)
	if m == nil {
		return ""
	}
	v := strings.TrimSpace(m[1])
	if strings.EqualFold(v, "none") {
		return ""
	}
	return v
}

// parseSSIDFromIpconfigSummary reads `ipconfig getsummary <iface>`
// output. macOS daemons without Location Services permission get a
// "<redacted>" placeholder instead of the real SSID; treat that as
// absent so the next fallback can try.
func parseSSIDFromIpconfigSummary(out string) string {
	m := ssidRe.FindStringSubmatch(out)
	if m == nil {
		return ""
	}
	v := strings.TrimSpace(m[1])
	if strings.Contains(v, "redacted") {
		return ""
	}
	return v
}

// parseSSIDFromIwLink reads `iw dev <iface> link` output. When
// disconnected the command prints "Not connected." and no SSID line.
func parseSSIDFromIwLink(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, "SSID: "); i >= 0 {
			return strings.TrimSpace(line[i+6:])
		}
	}
	return ""
}

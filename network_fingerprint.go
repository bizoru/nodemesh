package main

import (
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Network fingerprint: default-gateway MAC and local IPv4 subnet, the two
// pieces of local network identity the city-watchdog needs to cluster nodes
// by LAN. Public IPv4 is already published separately via [Egress]; these
// fields complete the triangulation (publicIP + gatewayMAC + localSubnet).
//
// Pure parsers live here with no system dependencies so they can be unit
// tested from canned fixtures. The per-OS wrappers in
// network_fingerprint_{darwin,linux,windows}.go run exec (darwin, Windows)
// or read /proc (Linux, Android-safe) and hand the output to these parsers.

// normalizeMAC returns a MAC in lowercase colon form. Accepts colon or
// dash separators (both show up in the wild: arp uses colons, netsh dashes).
// Returns "" when the input isn't a valid 48-bit MAC.
func normalizeMAC(raw string) string {
	raw = strings.TrimSpace(strings.ReplaceAll(raw, "-", ":"))
	if raw == "" {
		return ""
	}
	hw, err := net.ParseMAC(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(hw.String())
}

// ---------- darwin parsers ----------

// parseGatewayIPFromDarwinRoute reads `route -n get default` output and
// returns the gateway IPv4, or "" if no gateway is listed.
//
//	   route to: default
//	destination: default
//	       mask: default
//	    gateway: 192.168.1.1
func parseGatewayIPFromDarwinRoute(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "gateway:") {
			continue
		}
		ip := strings.TrimSpace(strings.TrimPrefix(line, "gateway:"))
		if parsed := net.ParseIP(ip); parsed != nil && parsed.To4() != nil {
			return parsed.To4().String()
		}
	}
	return ""
}

// parseMACFromDarwinArp reads `arp -an <gwIP>` output and returns the MAC
// of the row matching gwIP, normalized to lowercase colon form.
//
//	? (192.168.1.1) at a4:83:e7:aa:bb:cc on en0 ifscope [ethernet]
func parseMACFromDarwinArp(out, gwIP string) string {
	needle := "(" + gwIP + ")"
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, needle) {
			continue
		}
		at := strings.Index(line, " at ")
		if at < 0 {
			continue
		}
		rest := line[at+4:]
		on := strings.Index(rest, " on ")
		if on < 0 {
			continue
		}
		return normalizeMAC(strings.TrimSpace(rest[:on]))
	}
	return ""
}

// parseLocalSubnetFromDarwinIfconfig reads `ifconfig <iface>` output and
// returns the IPv4 subnet in CIDR form (e.g. "192.168.1.0/24"), or "" if
// the iface section has no `inet <ip> netmask 0xHHHHHHHH` line.
func parseLocalSubnetFromDarwinIfconfig(out, iface string) string {
	inIface := false
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, iface+":") {
			inIface = true
			continue
		}
		if inIface && !strings.HasPrefix(line, "\t") && !strings.HasPrefix(line, " ") && line != "" {
			// A new iface section started.
			break
		}
		if !inIface {
			continue
		}
		fields := strings.Fields(line)
		var ip, mask string
		for i, f := range fields {
			if f == "inet" && i+1 < len(fields) {
				ip = fields[i+1]
			}
			if f == "netmask" && i+1 < len(fields) {
				mask = fields[i+1]
			}
		}
		if ip == "" || mask == "" {
			continue
		}
		return subnetFromIPAndHexMask(ip, mask)
	}
	return ""
}

func subnetFromIPAndHexMask(ip, hexMask string) string {
	hexMask = strings.TrimPrefix(hexMask, "0x")
	mb, err := hex.DecodeString(hexMask)
	if err != nil || len(mb) != 4 {
		return ""
	}
	v4 := net.ParseIP(ip).To4()
	if v4 == nil {
		return ""
	}
	mask := net.IPv4Mask(mb[0], mb[1], mb[2], mb[3])
	ones, bits := mask.Size()
	if bits != 32 {
		return ""
	}
	return v4.Mask(mask).String() + "/" + strconv.Itoa(ones)
}

// ---------- linux parsers ----------

// parseGatewayFromLinuxProcRoute reads /proc/net/route and returns
// (gatewayIPv4, iface) for the default route, or ("","") if there isn't one.
//
// Format (tab separated, hex fields in little-endian):
//
//	Iface  Destination  Gateway  Flags  RefCnt  Use  Metric  Mask  MTU  Window  IRTT
//	wlan0  00000000     0101A8C0 0003   0       0    0       00000000 0  0       0
func parseGatewayFromLinuxProcRoute(contents string) (string, string) {
	for i, line := range strings.Split(contents, "\n") {
		if i == 0 || line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		if fields[1] != "00000000" {
			continue
		}
		gw := hexLEToIPv4(fields[2])
		if gw == "" || gw == "0.0.0.0" {
			continue
		}
		return gw, fields[0]
	}
	return "", ""
}

// parseMACFromLinuxProcARP reads /proc/net/arp and returns the MAC of the
// row for gwIP, normalized to lowercase colon form.
//
//	IP address       HW type   Flags   HW address         Mask   Device
//	192.168.1.1      0x1       0x2     aa:bb:cc:dd:ee:ff  *      wlan0
func parseMACFromLinuxProcARP(contents, gwIP string) string {
	for i, line := range strings.Split(contents, "\n") {
		if i == 0 || line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 6 || fields[0] != gwIP {
			continue
		}
		return normalizeMAC(fields[3])
	}
	return ""
}

// parseLocalSubnetFromLinuxProcRoute returns the local IPv4 subnet for
// iface in CIDR form, or "" if no on-link row exists for that iface.
// On-link rows have Gateway=00000000 and Destination/Mask fields set.
func parseLocalSubnetFromLinuxProcRoute(contents, iface string) string {
	for i, line := range strings.Split(contents, "\n") {
		if i == 0 || line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 8 || fields[0] != iface {
			continue
		}
		if fields[1] == "00000000" || fields[2] != "00000000" {
			continue
		}
		destIP := hexLEToIPv4(fields[1])
		maskIP := hexLEToIPv4(fields[7])
		if destIP == "" || maskIP == "" {
			continue
		}
		mb := strings.Split(maskIP, ".")
		if len(mb) != 4 {
			continue
		}
		bb := make([]byte, 4)
		for j, p := range mb {
			n, err := strconv.Atoi(p)
			if err != nil || n < 0 || n > 255 {
				bb = nil
				break
			}
			bb[j] = byte(n)
		}
		if bb == nil {
			continue
		}
		mask := net.IPv4Mask(bb[0], bb[1], bb[2], bb[3])
		ones, _ := mask.Size()
		return destIP + "/" + strconv.Itoa(ones)
	}
	return ""
}

// hexLEToIPv4 decodes an 8-char little-endian hex string (the format
// /proc/net/route uses for IPv4 fields) into dotted-quad form.
func hexLEToIPv4(h string) string {
	if len(h) != 8 {
		return ""
	}
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != 4 {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d.%d", b[3], b[2], b[1], b[0])
}

package main

import "os"

// Linux collectors: zero exec (see netspeed_linux.go header). On Android
// (R1) a single `exec` call aborts the whole service via SIGSYS once
// seccomp kicks in, so these read /proc directly.

func collectGatewayMAC() string {
	routeBytes, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}
	gw, _ := parseGatewayFromLinuxProcRoute(string(routeBytes))
	if gw == "" {
		return ""
	}
	arpBytes, err := os.ReadFile("/proc/net/arp")
	if err != nil {
		return ""
	}
	return parseMACFromLinuxProcARP(string(arpBytes), gw)
}

func collectLocalSubnet() string {
	routeBytes, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}
	_, iface := parseGatewayFromLinuxProcRoute(string(routeBytes))
	if iface == "" {
		return ""
	}
	return parseLocalSubnetFromLinuxProcRoute(string(routeBytes), iface)
}

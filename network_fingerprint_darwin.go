package main

// Darwin collectors: exec is cheap here (no SIGSYS risk like Android/R1),
// and `route`/`arp`/`ifconfig` are the same tools netspeed_darwin.go
// already uses. No sudo required.

func collectGatewayMAC() string {
	gw := parseGatewayIPFromDarwinRoute(run("route", "-n", "get", "default"))
	if gw == "" {
		return ""
	}
	return parseMACFromDarwinArp(run("arp", "-an", gw), gw)
}

func collectLocalSubnet() string {
	iface := defaultIface()
	if iface == "" {
		return ""
	}
	return parseLocalSubnetFromDarwinIfconfig(run("ifconfig", iface), iface)
}

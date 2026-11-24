package main

// Windows: nodemesh does run on Windows (legion, athena's WSL host would
// use the Linux build), but today no city-watchdog cluster lives behind a
// Windows node. Stub these so the fields serialize empty; the parser
// unit tests still exercise darwin and linux logic. Fill in with
// `route print` / `arp -a` parsing when a Windows node needs to anchor a
// cluster.

func collectGatewayMAC() string  { return "" }
func collectLocalSubnet() string { return "" }

//go:build !windows

package main

import "net"

// Fuera de Windows no hay sondeo ARP propio: en Linux/macOS haría falta root
// para inyectar tramas, y ahí el vecino que importa (athena) no vive. Los
// llamantes caen al ping, que es lo que había antes de todo esto.
func alcanzaPorARP(ip net.IP) string { return "" }

func buscaPorMAC(base net.IP, mac string) string { return "" }

//go:build windows

package main

import (
	"encoding/binary"
	"net"
	"sync"
	"syscall"
	"unsafe"
)

// Sondeo de vecindad por ARP. Existe porque el ping NO sirve para esto:
//
// El 2026-09-21, desde rigby, athena no respondía a ICMP ni a la mayoría de
// puertos TCP —su firewall tiene `LocalFirewallRules: N/A (GPO-store only)`,
// así que las reglas locales no se aplican— pero SÍ respondía a ARP con
// `E0-2E-0B-92-C0-8E`. Y tiene que ser así: ARP lo contesta la pila de red por
// debajo del cortafuegos, que filtra IP, no Ethernet.
//
// Consecuencia: una máquina ENCENDIDA en la LAN siempre contesta ARP aunque
// esté cerrada a cal y canto. Para la pregunta "¿el vecino está vivo?" eso es
// justo lo que se quiere, y el ping da un falso negativo.
//
// Solo vale dentro de la MISMA subred: ARP no cruza routers.

var (
	iphlpapi    = syscall.NewLazyDLL("iphlpapi.dll")
	procSendARP = iphlpapi.NewProc("SendARP")
)

// alcanzaPorARP devuelve la MAC del destino, o "" si no contesta.
func alcanzaPorARP(ip net.IP) string {
	v4 := ip.To4()
	if v4 == nil {
		return ""
	}
	// SendARP toma la IPv4 como DWORD con los bytes en el mismo orden en que
	// están en memoria; en little-endian eso es exactamente LittleEndian.Uint32.
	destino := binary.LittleEndian.Uint32(v4)
	var mac [8]byte
	largo := uint32(len(mac))
	r, _, _ := procSendARP.Call(
		uintptr(destino), 0,
		uintptr(unsafe.Pointer(&mac[0])),
		uintptr(unsafe.Pointer(&largo)),
	)
	if r != 0 || largo != 6 {
		return ""
	}
	return net.HardwareAddr(mac[:6]).String()
}

// buscaPorMAC barre la /24 de `base` buscando una MAC concreta y devuelve la IP
// donde está, o "".
//
// Es la respuesta a que athena volviera con .92 en vez de .93 y se llevara por
// delante la regla de firewall y la lista de peers que la nombraban por IP: la
// identidad de una máquina es su MAC, la dirección es un detalle que el DHCP
// cambia cuando le parece.
//
// Caro a propósito de usar poco: SendARP tarda ~2 s en cada IP muerta, así que
// se barre con 24 en paralelo y solo cuando el sondeo directo ya ha fallado.
func buscaPorMAC(base net.IP, mac string) string {
	v4 := base.To4()
	if v4 == nil || mac == "" {
		return ""
	}
	objetivo := normMAC(mac)
	var (
		mu         sync.Mutex
		encontrada string
		wg         sync.WaitGroup
	)
	hueco := make(chan struct{}, 24)
	for i := 1; i <= 254; i++ {
		ip := net.IPv4(v4[0], v4[1], v4[2], byte(i))
		wg.Add(1)
		go func(ip net.IP) {
			defer wg.Done()
			hueco <- struct{}{}
			defer func() { <-hueco }()
			mu.Lock()
			ya := encontrada != ""
			mu.Unlock()
			if ya {
				return
			}
			if m := alcanzaPorARP(ip); m != "" && normMAC(m) == objetivo {
				mu.Lock()
				if encontrada == "" {
					encontrada = ip.String()
				}
				mu.Unlock()
			}
		}(ip)
	}
	wg.Wait()
	return encontrada
}

package main

import (
	"os"
	"regexp"
	"strconv"
	"strings"
)

// media: autoselect (1000baseT <full-duplex>) — el número antes de "base" es
// la velocidad. La G de "10Gbase-T" multiplica por mil.
var mediaRe = regexp.MustCompile(`(?i)\b(\d+)(G)?base`)

// wdutil imprime "Tx Rate : 866.700 (Mbps)" y "RSSI : -52 (dBm)".
var (
	wdTxRateRe = regexp.MustCompile(`(?i)Tx Rate\s*:\s*([\d.]+)`)
	wdRSSIRe   = regexp.MustCompile(`(?i)RSSI\s*:\s*(-?\d+)`)
)

func platformNet() (NetLink, uint64, uint64) {
	n := NetLink{}
	iface := defaultIface()
	if iface == "" {
		return n, 0, 0
	}
	n.Iface = iface

	if m := mediaRe.FindStringSubmatch(run("ifconfig", iface)); m != nil {
		if v, err := strconv.Atoi(m[1]); err == nil && v > 0 {
			if strings.EqualFold(m[2], "G") {
				v *= 1000
			}
			n.LinkMbps, n.LinkSource = v, "ifconfig"
		}
	}

	// En wifi, ifconfig dice "autoselect" y no da tasa: la real la sabe
	// wdutil, pero SÓLO como root. Es la misma asimetría que ya sufre el SSID
	// en netStatus(), y con el mismo reparto en la flota: los Macs que corren
	// nodemesh como LaunchDaemon (mini, lia) publican tasa y RSSI; el
	// MacBook Air, que corre como LaunchAgent porque allí sudo pide clave,
	// se queda sin ellos y vive con el tráfico observado. Preferible a
	// llamar a system_profiler cada 30 s, que tarda segundos.
	if os.Geteuid() == 0 {
		out := run("wdutil", "info")
		if m := wdRSSIRe.FindStringSubmatch(out); m != nil {
			if v, err := strconv.Atoi(m[1]); err == nil && v < 0 {
				n.RSSI = v
			}
		}
		if m := wdTxRateRe.FindStringSubmatch(out); m != nil {
			if v, err := strconv.ParseFloat(m[1], 64); err == nil && v > 0 {
				n.LinkMbps, n.LinkSource = int(v), "wdutil"
			}
		}
	}

	rx, tx := netstatCounters(iface)
	return n, rx, tx
}

// netstatCounters saca los bytes acumulados de `netstat -ibn -I <iface>`. La
// interfaz aparece en varias filas (una por familia de direcciones) con los
// MISMOS acumulados; se usa la de <Link#N>, que es la de la interfaz física.
//
//	Name  Mtu   Network     Address        Ipkts Ierrs  Ibytes  Opkts Oerrs  Obytes  Coll
//	en0   1500  <Link#12>   a4:83:e7:..   123456     0  9876543  65432     0  123456     0
//
// Aquí sí se lanza un proceso: en macOS los contadores viven en un struct
// binario que el paquete syscall no expone (haría falta x/sys, y este binario
// no tiene dependencias externas a propósito). Es un netstat cada 30 s en
// máquinas que ya pagan un vm_stat en el mismo ciclo, y macOS no tiene el
// problema de Android con exec.
func netstatCounters(iface string) (uint64, uint64) {
	for _, line := range strings.Split(run("netstat", "-ibn", "-I", iface), "\n") {
		f := strings.Fields(line)
		// Una interfaz sin MAC no imprime columna Address, así que la fila
		// queda con un campo menos: se localizan las columnas contando desde
		// Network, que es la que siempre está.
		if len(f) < 10 || f[0] != iface || !strings.HasPrefix(f[2], "<Link") {
			continue
		}
		off := 0
		if len(f) >= 11 {
			off = 1 // hay columna Address
		}
		rx, err1 := strconv.ParseUint(f[5+off], 10, 64)
		tx, err2 := strconv.ParseUint(f[8+off], 10, 64)
		if err1 != nil || err2 != nil {
			return 0, 0
		}
		return rx, tx
	}
	return 0, 0
}

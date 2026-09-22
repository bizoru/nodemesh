package main

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// En Linux esto es CERO exec, igual que specs_linux.go y por el mismo motivo:
// el R1 corre Android, donde lanzar un proceso mata el servicio entero con
// SIGSYS (ver la cabecera de specs_linux.go). Todo sale de /sys, /proc y un
// ioctl — un ioctl sobre un socket ya abierto no pasa por faccessat2, así que
// es seguro incluso ahí.
func platformNet() (NetLink, uint64, uint64) {
	n := NetLink{}
	iface := defaultIface()
	if iface == "" {
		return n, 0, 0
	}
	n.Iface = iface
	base := "/sys/class/net/" + iface

	// speed está en Mb/s y lo publica el driver vía ethtool. En ethernet de
	// metal da el número negociado (100, 1000, 2500); en wifi y en las
	// virtio de los VPS suele dar -1 o "Unknown!", que es una respuesta
	// honesta —el driver no lo sabe— y aquí se traduce a 0.
	if mbps, err := strconv.Atoi(firstFileLine(base + "/speed")); err == nil && mbps > 0 {
		n.LinkMbps, n.LinkSource = mbps, "sysfs"
	}

	if _, err := os.Stat(base + "/wireless"); err == nil {
		n.RSSI = wirelessRSSI(iface)
		if n.LinkMbps == 0 {
			if mbps := wextBitrate(iface); mbps > 0 {
				n.LinkMbps, n.LinkSource = mbps, "wext"
			}
		}
	}

	return n, readCounterFile(base + "/statistics/rx_bytes"),
		readCounterFile(base + "/statistics/tx_bytes")
}

func readCounterFile(path string) uint64 {
	v, _ := strconv.ParseUint(firstFileLine(path), 10, 64)
	return v
}

// wirelessRSSI lee el nivel de señal de /proc/net/wireless. La línea es
//
//	wlan0: 0000   70.  -40.  -256        0      0 ...
//
// y las tres cifras tras el estado son calidad, nivel y ruido; el punto final
// es parte del formato, no un decimal.
//
// Sólo se acepta un valor NEGATIVO. El nivel debería ser dBm, pero hay drivers
// viejos que publican ahí un 0-255 relativo, y un "RSSI de 154" mezclado con
// los dBm reales del resto de la flota sería peor que no tener el dato.
func wirelessRSSI(iface string) int {
	b, err := os.ReadFile("/proc/net/wireless")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || strings.TrimSuffix(f[0], ":") != iface {
			continue
		}
		v, err := strconv.Atoi(strings.TrimSuffix(f[3], "."))
		if err != nil || v >= 0 {
			return 0
		}
		return v
	}
	return 0
}

// SIOCGIWRATE: ioctl de las Wireless Extensions que devuelve la tasa de
// transmisión actual en bits/s. Es la API vieja, pero cfg80211 la mantiene por
// compatibilidad y la sirve cualquier kernel que tenga /proc/net/wireless —
// que es justo la condición con la que se llama aquí.
//
// Se usa esto y no `iw dev X link` a propósito: `iw` es un proceso, y en
// Android lanzar un proceso mata el servicio. Un ioctl no.
const siocGIWRATE = 0x8B21

// iwreq es `struct iwreq`: el nombre de la interfaz y una unión. Para
// SIOCGIWRATE el kernel escribe en ella un `struct iw_param`, cuyos primeros
// cuatro bytes son el valor con signo; el resto de la unión se reserva de
// sobra para que el kernel nunca escriba fuera.
type iwreq struct {
	name [16]byte
	data [40]byte
}

func wextBitrate(iface string) int {
	if len(iface) >= 16 {
		return 0
	}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return 0
	}
	defer syscall.Close(fd)

	var req iwreq
	copy(req.name[:], iface)
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), siocGIWRATE,
		uintptr(unsafe.Pointer(&req)))
	if errno != 0 {
		// EOPNOTSUPP en drivers que ya no traen la compatibilidad WEXT, EPERM
		// donde SELinux la cierra (Android). No es un fallo que reportar: el
		// nodo sigue publicando su RSSI y su tráfico observado.
		return 0
	}
	bps := *(*int32)(unsafe.Pointer(&req.data[0]))
	if bps <= 0 {
		return 0
	}
	return int(bps / 1_000_000)
}

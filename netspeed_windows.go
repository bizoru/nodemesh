package main

import (
	"strings"
	"unsafe"
)

// iphlpapi ya esta declarado en arp_windows.go (mismo paquete), igual que
// kernel32 lo esta en uptime_windows.go.
var (
	procGetBestInterface = iphlpapi.NewProc("GetBestInterface")
	procGetIfEntry       = iphlpapi.NewProc("GetIfEntry")
)

const (
	maxInterfaceNameLen = 256
	maxlenPhysaddr      = 8
	maxlenIfdescr       = 256
	ifOperStatusUp      = 5 // MIB_IF_OPER_STATUS_OPERATIONAL
)

// mibIfRow es MIB_IFROW, la estructura VIEJA de iphlpapi. Se usa ésta y no
// MIB_IF_ROW2 por dos razones: es plana —todo DWORDs alineados a 4, así que el
// mapeo a Go no depende de adivinar el relleno de una unión ni de un campo de
// bits, que es donde estas traducciones fallan en silencio y devuelven
// números creíbles pero falsos— y existe desde Windows 2000, con lo que sirve
// también en rigby, que es Windows 8.1 de 32 bits.
//
// Lo que se paga a cambio: dwInOctets/dwOutOctets son de 32 bits y desbordan
// cada 4 GB. A 30 s de ventana eso es 1,1 Gb/s sostenidos, que ningún nodo de
// esta flota alcanza; y si pasara, el delta sale negativo y localNetLink() lo
// descarta en vez de publicar basura.
//
// Nada de esto lanza un proceso. Es deliberado: athena va justa de RAM y rigby
// es un Atom con 1 GB, y arrancar un powershell cada 30 s costaría más que el
// dato que produce (mismo criterio que platformStatic en specs_windows.go).
type mibIfRow struct {
	Name            [maxInterfaceNameLen]uint16
	Index           uint32
	Type            uint32
	Mtu             uint32
	Speed           uint32 // bits/s
	PhysAddrLen     uint32
	PhysAddr        [maxlenPhysaddr]byte
	AdminStatus     uint32
	OperStatus      uint32
	LastChange      uint32
	InOctets        uint32
	InUcastPkts     uint32
	InNUcastPkts    uint32
	InDiscards      uint32
	InErrors        uint32
	InUnknownProtos uint32
	OutOctets       uint32
	OutUcastPkts    uint32
	OutNUcastPkts   uint32
	OutDiscards     uint32
	OutErrors       uint32
	OutQLen         uint32
	DescrLen        uint32
	Descr           [maxlenIfdescr]byte
}

func platformNet() (NetLink, uint64, uint64) {
	n := NetLink{}

	// GetBestInterface responde con el índice de la interfaz por la que
	// saldría un paquete hacia ese destino: el equivalente al defaultIface()
	// de unix, pero preguntándoselo a la pila en vez de leer la tabla de
	// rutas. 8.8.8.8 es el mismo destino que ya usa localIP().
	var idx uint32
	r, _, _ := procGetBestInterface.Call(
		uintptr(0x08080808), // 8.8.8.8 en orden de red
		uintptr(unsafe.Pointer(&idx)))
	if r != 0 {
		return n, 0, 0
	}

	row := mibIfRow{Index: idx}
	if r, _, _ := procGetIfEntry.Call(uintptr(unsafe.Pointer(&row))); r != 0 {
		return n, 0, 0
	}
	if row.OperStatus != ifOperStatusUp {
		return n, 0, 0
	}

	// La descripción ("Intel(R) Wi-Fi 6 AX201") identifica la interfaz mucho
	// mejor que wszName, que es \DEVICE\TCPIP_{GUID} y no le dice nada a nadie.
	if l := int(row.DescrLen); l > 0 && l <= len(row.Descr) {
		n.Iface = strings.TrimRight(string(row.Descr[:l]), "\x00")
	}
	if n.Iface == "" {
		// Sin nombre no hay muestra utilizable: localNetLink() compara la
		// interfaz entre rondas para no calcular tasas a caballo de un cambio
		// de red, y un nombre vacío haría que wifi y ethernet parecieran la
		// misma.
		return n, 0, 0
	}
	if row.Speed > 0 {
		n.LinkMbps, n.LinkSource = int(row.Speed/1_000_000), "ifentry"
	}
	return n, uint64(row.InOctets), uint64(row.OutOctets)
}

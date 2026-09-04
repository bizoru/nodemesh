package main

import (
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// kernel32 ya está declarado en uptime_windows.go (mismo paquete).
var (
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetDiskFreeSpaceExW  = kernel32.NewProc("GetDiskFreeSpaceExW")
)

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// platformStatic es el ÚNICO sitio donde Windows paga un powershell, y solo
// una vez por proceso: specs.go cachea lo estático con un sync.Once. Lo
// volátil (memoria, disco) va por kernel32 y no lanza procesos nunca —
// arrancar powershell cada 30 s en athena, que ya va justa de RAM, sería
// peor que el dato que produce.
func platformStatic() Specs {
	s := Specs{OS: "windows"}
	out := run("powershell", "-NoProfile", "-NonInteractive", "-Command",
		`$c=@(Get-CimInstance Win32_Processor)[0]; $s=Get-CimInstance Win32_ComputerSystem; `+
			`$o=Get-CimInstance Win32_OperatingSystem; `+
			`"cpu="+$c.Name; "phys="+$c.NumberOfCores; "log="+$c.NumberOfLogicalProcessors; `+
			`"model="+$s.Manufacturer+" "+$s.Model; "os="+$o.Caption+" "+$o.Version`)
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "cpu":
			s.CPU = v
		case "phys":
			s.CoresPhys, _ = strconv.Atoi(v)
		case "log":
			s.CoresLog, _ = strconv.Atoi(v)
		case "model":
			s.Model = v
		case "os":
			if v != "" {
				s.OS = v
			}
		}
	}
	if m, ok := memStatus(); ok {
		s.MemTotalMB = int64(m.TotalPhys >> 20)
	}
	return s
}

func memStatus() (memoryStatusEx, bool) {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	return m, r != 0
}

func memUsedMB() int64 {
	m, ok := memStatus()
	if !ok || m.TotalPhys < m.AvailPhys {
		return 0
	}
	return int64((m.TotalPhys - m.AvailPhys) >> 20)
}

// GetDiskFreeSpaceExW acepta cualquier directorio y responde por el volumen
// que lo contiene, así que el DataDir sirve tal cual.
func diskGB(dir string) (total, free float64) {
	if dir == "" {
		dir = `C:\`
	}
	path, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return 0, 0
	}
	var avail, totalBytes, totalFree uint64
	r, _, _ := procGetDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(path)),
		uintptr(unsafe.Pointer(&avail)),
		uintptr(unsafe.Pointer(&totalBytes)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if r == 0 {
		return 0, 0
	}
	const gb = 1 << 30
	return float64(totalBytes) / gb, float64(avail) / gb
}

// Windows no tiene loadavg y no hay equivalente honesto de "cola de
// ejecutables en el último minuto", así que se deja en cero en vez de
// inventar un número que no significa lo mismo que en los demás nodos.
func load1() float64 { return 0 }

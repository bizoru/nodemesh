package main

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// En Linux TODO sale de /proc, /sys y syscalls: cero exec.
//
// No es puritanismo. El R1 corre Android, y ahí exec.LookPath acaba llamando a
// faccessat2(), un syscall que el seccomp del sistema BLOQUEA: el proceso se
// muere con SIGSYS, que no se puede atrapar, y el supervisor lo revive para
// volver a morir (el crash-loop de 2026-09-01, ver termuxBin en host.go).
// Recolectar specs es algo que pasa cada 30 s en todos los nodos, así que este
// camino tiene que ser incapaz de disparar eso.
func platformStatic() Specs {
	s := Specs{}
	// uname(2) y no /proc/sys/kernel/osrelease: en Android ese fichero da
	// "Permission denied" (comprobado en el R1) aunque el syscall sí responda.
	var u syscall.Utsname
	if err := syscall.Uname(&u); err == nil {
		s.OS = "linux " + charsToString(u.Release[:])
	}
	s.Model = firstFileLine(
		"/sys/devices/virtual/dmi/id/product_name",
		"/proc/device-tree/model", // ARM/Android: SBCs y teléfonos no tienen DMI
		"/sys/firmware/devicetree/base/model",
	)
	s.CPU, s.CoresPhys, s.CoresLog = cpuInfo()
	if kb := meminfoKB("MemTotal"); kb > 0 {
		s.MemTotalMB = kb / 1024
	}
	return s
}

// firstFileLine devuelve la primera línea del primer fichero que exista y
// tenga contenido. Los del device-tree acaban en NUL, que hay que recortar o
// se cuela en el JSON.
// charsToString convierte los campos de utsname (arrays de int8/uint8 según
// la arquitectura) en string, cortando en el primer NUL.
func charsToString(c []int8) string {
	b := make([]byte, 0, len(c))
	for _, v := range c {
		if v == 0 {
			break
		}
		b = append(b, byte(v))
	}
	return string(b)
}

func firstFileLine(paths ...string) string {
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		v := strings.TrimSpace(strings.TrimRight(string(b), "\x00"))
		if i := strings.IndexByte(v, '\n'); i >= 0 {
			v = v[:i]
		}
		if v != "" {
			return v
		}
	}
	return ""
}

// cpuInfo saca modelo y núcleos de /proc/cpuinfo. Los físicos son los pares
// (physical id, core id) distintos; en ARM esos campos no existen, así que se
// devuelve 0 y specs.go cae a los lógicos.
func cpuInfo() (model string, phys, logical int) {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return "", 0, 0
	}
	cores := map[string]bool{}
	var pkg, core string
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			// línea en blanco: se cierra el procesador anterior
			if pkg != "" || core != "" {
				cores[pkg+"/"+core] = true
				pkg, core = "", ""
			}
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "processor":
			logical++
		case "model name", "Model", "Hardware", "cpu model":
			// En ARM no hay "model name"; "Hardware" (MT6765 en el R1) es lo
			// más parecido que publica el kernel.
			if model == "" {
				model = v
			}
		case "physical id":
			pkg = v
		case "core id":
			core = v
		}
	}
	if pkg != "" || core != "" {
		cores[pkg+"/"+core] = true
	}
	delete(cores, "/")
	return model, len(cores), logical
}

func meminfoKB(key string) int64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok || k != key {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			return 0
		}
		n, _ := strconv.ParseInt(f[0], 10, 64)
		return n
	}
	return 0
}

// memUsedMB usa MemAvailable, no MemFree: el caché de página cuenta como
// libre a efectos prácticos, y MemFree daría todos los servidores al 95%.
func memUsedMB() int64 {
	total, avail := meminfoKB("MemTotal"), meminfoKB("MemAvailable")
	if total == 0 {
		return 0
	}
	if avail == 0 { // kernels viejos sin MemAvailable
		avail = meminfoKB("MemFree") + meminfoKB("Cached") + meminfoKB("Buffers")
	}
	if avail > total {
		avail = total
	}
	return (total - avail) / 1024
}

func diskGB(path string) (total, free float64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	bs := float64(st.Bsize)
	const gb = 1 << 30
	return float64(st.Blocks) * bs / gb, float64(st.Bavail) * bs / gb
}

func load1() float64 {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

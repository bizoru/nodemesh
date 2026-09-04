package main

import (
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

func platformStatic() Specs {
	s := Specs{}
	s.Model, _ = syscall.Sysctl("hw.model")
	s.CPU, _ = syscall.Sysctl("machdep.cpu.brand_string")
	if rel, err := syscall.Sysctl("kern.osrelease"); err == nil {
		s.OS = "macOS (darwin " + rel + ")"
	}
	if v, err := syscall.SysctlUint32("hw.physicalcpu"); err == nil {
		s.CoresPhys = int(v)
	}
	if v, err := syscall.SysctlUint32("hw.logicalcpu"); err == nil {
		s.CoresLog = int(v)
	}
	// hw.memsize es de 64 bits y el paquete syscall solo expone SysctlUint32
	// (SysctlUint64/SysctlRaw viven en x/sys/unix, y este binario no tiene
	// dependencias externas a propósito). Un exec de sysctl(1) resuelve el
	// caso, y solo se paga una vez por proceso: platformStatic va tras un
	// sync.Once.
	if v, err := strconv.ParseInt(strings.TrimSpace(run("sysctl", "-n", "hw.memsize")), 10, 64); err == nil {
		s.MemTotalMB = v >> 20
	}
	return s
}

var pageSizeRe = regexp.MustCompile(`page size of (\d+) bytes`)

// memUsedMB replica lo que el Monitor de Actividad llama "memoria usada":
// páginas activas + wired + comprimidas. Las libres, inactivas y especulativas
// se pueden reclamar, así que contarlas daría un Mac "lleno" siempre.
func memUsedMB() int64 {
	out := run("vm_stat")
	if out == "" {
		return 0
	}
	pageSize := int64(4096)
	if m := pageSizeRe.FindStringSubmatch(out); m != nil {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil && n > 0 {
			pageSize = n
		}
	}
	want := map[string]int64{
		"Pages active":                 0,
		"Pages wired down":             0,
		"Pages occupied by compressor": 0,
	}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if _, need := want[k]; !need {
			continue
		}
		if n, err := strconv.ParseInt(strings.Trim(strings.TrimSpace(v), "."), 10, 64); err == nil {
			want[k] = n
		}
	}
	pages := want["Pages active"] + want["Pages wired down"] + want["Pages occupied by compressor"]
	return pages * pageSize >> 20
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

var loadRe = regexp.MustCompile(`([\d.]+)`)

func load1() float64 {
	// vm.loadavg sale como "{ 2.10 2.32 2.51 }"; sysctl(3) lo devuelve como
	// struct binario, así que aquí sí conviene el binario de línea de comandos.
	m := loadRe.FindStringSubmatch(run("sysctl", "-n", "vm.loadavg"))
	if m == nil {
		return 0
	}
	v, _ := strconv.ParseFloat(m[1], 64)
	return v
}

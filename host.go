package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// Battery is volatile host info served realtime (not chained).
//
// Charging significa "en corriente externa", no "cargando el porcentaje":
// lo que le importa a quien vigila es si el equipo SE ESTÁ DESCARGANDO o
// no. Un portátil enchufado al 100% ("charged", "Full", BatteryStatus=2)
// cuenta como Charging=true en las tres plataformas.
type Battery struct {
	Present  bool `json:"present"`
	Percent  int  `json:"percent"`
	Charging bool `json:"charging"`
}

type HostInfo struct {
	Battery Battery `json:"battery"`
}

var pctRe = regexp.MustCompile(`(\d+)%`)

func battery() Battery {
	switch runtime.GOOS {
	case "darwin":
		out := run("pmset", "-g", "batt")
		if !strings.Contains(out, "InternalBattery") {
			return Battery{} // desktop / no battery
		}
		b := Battery{Present: true}
		if m := pctRe.FindStringSubmatch(out); m != nil {
			b.Percent, _ = strconv.Atoi(m[1])
		}
		// pmset NUNCA imprime "AC attached" — esa cadena era de otra
		// herramienta y no aparecía jamás, así que TODO Mac enchufado se
		// reportaba descargando (a plugged-in Mac reported 100% and charging:false).
		// La cabecera real dice literalmente:
		//   Now drawing from 'AC Power'   |   Now drawing from 'Battery Power'
		b.Charging = strings.Contains(out, "'AC Power'")
		if strings.Contains(out, "'Battery Power'") {
			b.Charging = false
		}
		return b
	case "linux":
		if b, ok := batterySysfs(); ok {
			return b
		}
		// Android/Termux: /sys/class/power_supply no se puede
		// leer sin root, pero Termux:API sí expone la batería.
		if b, ok := batteryTermux(); ok {
			return b
		}
		return Battery{}
	case "windows":
		// WMIC BatteryStatus: 2 = on AC. EstimatedChargeRemaining = percent.
		out := run("powershell", "-NoProfile", "-Command",
			"(Get-CimInstance Win32_Battery | Select-Object -First 1 EstimatedChargeRemaining,BatteryStatus | ConvertTo-Csv -NoTypeInformation)")
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) < 2 {
			return Battery{}
		}
		f := strings.Split(strings.Trim(lines[1], "\r\""), "\",\"")
		if len(f) < 2 {
			return Battery{}
		}
		b := Battery{Present: true}
		b.Percent, _ = strconv.Atoi(strings.TrimSpace(f[0]))
		b.Charging = strings.TrimSpace(f[1]) == "2"
		return b
	}
	return Battery{}
}

// batterySysfs lee la primera batería real de /sys/class/power_supply. Se
// recorre el directorio en vez de asumir BAT0: no todos los portátiles la
// numeran igual (BAT1 es común) y los cargadores aparecen ahí mismo como
// type=Mains.
func batterySysfs() (Battery, bool) {
	const root = "/sys/class/power_supply"
	entries, err := os.ReadDir(root)
	if err != nil {
		return Battery{}, false
	}
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		if t, err := os.ReadFile(filepath.Join(dir, "type")); err != nil ||
			strings.TrimSpace(string(t)) != "Battery" {
			continue
		}
		cap, err := os.ReadFile(filepath.Join(dir, "capacity"))
		if err != nil {
			continue
		}
		b := Battery{Present: true}
		b.Percent, _ = strconv.Atoi(strings.TrimSpace(string(cap)))
		st, _ := os.ReadFile(filepath.Join(dir, "status"))
		s := strings.TrimSpace(string(st))
		b.Charging = s == "Charging" || s == "Full" || s == "Not charging"
		return b, true
	}
	return Battery{}, false
}

// batteryTermux usa termux-battery-status (paquete termux-api + la app
// Termux:API). Si no está instalado devuelve ok=false y el nodo sigue
// reportando "sin batería", como antes.
func batteryTermux() (Battery, bool) {
	bin := termuxBin()
	if bin == "" {
		return Battery{}, false
	}
	out := run(bin)
	if strings.TrimSpace(out) == "" {
		return Battery{}, false
	}
	var t struct {
		Present    *bool  `json:"present"`
		Percentage int    `json:"percentage"`
		Status     string `json:"status"`
		Plugged    string `json:"plugged"`
	}
	if err := json.Unmarshal([]byte(out), &t); err != nil {
		return Battery{}, false
	}
	if t.Present != nil && !*t.Present {
		return Battery{}, false
	}
	b := Battery{Present: true, Percent: t.Percentage}
	// plugged: PLUGGED_AC / PLUGGED_USB / PLUGGED_WIRELESS / UNPLUGGED.
	// status FULL sin enchufe no existe en la práctica, pero se acepta.
	b.Charging = strings.HasPrefix(t.Plugged, "PLUGGED") ||
		t.Status == "CHARGING" || t.Status == "FULL"
	return b, true
}

// termuxBin devuelve la ruta ABSOLUTA de termux-battery-status, o "" si no
// está instalado. Nunca se busca por PATH: en Android, exec.LookPath acaba
// llamando faccessat2(), un syscall que el seccomp del sistema BLOQUEA, y
// el proceso entero se muere con SIGSYS — no es un error que se pueda
// atrapar, y el supervisor lo revive para volver a morir en la siguiente
// petición (crash-loop del R1, 2026-09-01). Con la ruta absoluta,
// exec.Command se salta LookPath y no hay syscall prohibido.
func termuxBin() string {
	var cands []string
	if p := os.Getenv("PREFIX"); p != "" {
		cands = append(cands, filepath.Join(p, "bin", "termux-battery-status"))
	}
	cands = append(cands, "/data/data/com.termux/files/usr/bin/termux-battery-status")
	for _, c := range cands {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

func hostHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, HostInfo{Battery: battery()})
}

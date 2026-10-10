package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// tailscaleBackend devuelve el BackendState (`Running`, `Stopped`, `NeedsLogin`,
// `NoState`, `Starting`…) de `tailscale status --json`. Vacío si tailscale no
// está instalado o no se puede determinar. Lo pide el latido para que el
// collector sepa POR QUÉ un nodo pudo quedar sin reportar antes de callarse —
// "la máquina vive, pero tailscale backend estaba Stopped" vs "la máquina se
// cayó" se ven idénticos desde fuera y son incidentes distintos. 2026-10-10.
//
// Gotcha macOS: cuando nodemesh corre como root (LaunchDaemon) sin sesión
// gráfica, `tailscale status --json` puede colgarse esperando a la GUI (ver
// comentario de run()). El timeout de run() lo corta a los 10 s y el CLI
// devuelve "". Para ese caso hay un fallback por tailscaleIP(): si el nodo
// tiene una IP CGNAT activa, el backend ESTÁ Running — una conclusión más
// burda que leer BackendState (perdemos "NeedsLogin"/"Starting" vs "Stopped")
// pero suficiente para la pregunta operativa clave "¿hay tailnet o no?".
func tailscaleBackend() string {
	bin := findTailscale()
	if bin != "" {
		// --peers=false para no serializar la vista completa del tailnet; el
		// único campo que leemos es BackendState.
		out := run(bin, "status", "--json", "--peers=false")
		if out != "" {
			var v struct {
				BackendState string
			}
			if err := json.Unmarshal([]byte(out), &v); err == nil && v.BackendState != "" {
				return v.BackendState
			}
		}
	}
	// Fallback: inferir del hecho de tener IP CGNAT. tailscaleIP() ya cubre
	// los caminos alternativos (fallback a net.InterfaceAddrs()) que
	// sobreviven a un CLI colgado.
	if tailscaleIP() != "" {
		return "Running"
	}
	return ""
}

// limaVMs devuelve "nombre:estado[,nombre:estado…]" con el estado de todas las
// VMs de Lima en este host (actualmente mini y lia hospedan workers k3s en VMs
// Lima). Vacío si limactl no está disponible o si no hay VMs. El collector lo
// guarda y lo muestra en las alertas: "lima=k3s:Broken" tras un reboot cubre
// justo el gap que dejó al descubierto el incidente del 9-oct-2026 — el HOST
// seguía latiendo mientras la VM llevaba 6 h caída y nada lo veía.
//
// Gotcha: nodemesh corre como root (LaunchDaemon), pero las VMs de Lima viven
// bajo el home del USUARIO que las creó (`~/.lima/<vm>/…`). Un `limactl list`
// invocado por root con HOME=/var/root no ve ninguna, devuelve una lista
// vacía SIN ERROR y aquí llegaríamos a "". Para que vea las VMs reales hay
// que cambiar UID al dueño del binario — por eso la implementación vive en
// supervision_unix.go (requiere syscall.Credential, no existe en Windows,
// que de todos modos no usa Lima).
func limaVMs() string {
	bin := findLimactl()
	if bin == "" {
		return ""
	}
	return limactlList(bin)
}

// localMAC devuelve la MAC de la interfaz por la que sale el tráfico default.
// La MAC viaja en el latido porque PUEDE CAMBIAR entre reboots (Private WiFi
// Address en macOS/iOS, randomización de MAC en Windows/Android), y cuando un
// nodo pierde tailnet el único camino de rescate es buscarlo en la LAN —si
// la nota de la entidad lleva una MAC vieja, la búsqueda falla antes de empezar.
// 2026-10-10, tras incidente mini.
//
// Gotcha de macOS Sonoma+ (Darwin 27 verificado): `net.Interfaces()` devuelve
// "02:00:00:00:00:00" en TODAS las interfaces desde un proceso no privilegiado
// —API de privacidad— así que la API de Go no sirve. Hay que leer de ifconfig
// o /sys/class/net según el SO. 2026-10-10.
func localMAC() string {
	iface := defaultIface()
	// defaultIface() en Windows devuelve "" porque route print es un parser
	// aparte; para la MAC no hace falta la iface "default" exacta, cualquier
	// NIC UP con HardwareAddr válida identifica la máquina en LAN. Si no hay
	// iface, el caso windows abajo enumera net.Interfaces() y toma la primera
	// UP no-loopback. En darwin/linux SIN iface default no hay cómo mirar sin
	// escanear, y no vale la pena.
	if iface == "" && runtime.GOOS != "windows" {
		return ""
	}
	switch runtime.GOOS {
	case "darwin":
		// 1) Prefiero ifconfig: devuelve la MAC asociada al link actual
		// (incluye Private WiFi Address rotada, que es la MAC con la que
		// otros dispositivos nos ven en la LAN — justo lo que hace falta
		// para rescate).
		//
		// Pero gotcha de macOS Sonoma+: un proceso NO privilegiado ve
		// "02:00:00:00:00:00" por API de privacidad. nodemesh en mini corre
		// como `steven.sierra` (no root) → cae en el caso censurado. En lia
		// corre como root → ve la MAC real. 2026-10-10.
		out := run("/sbin/ifconfig", iface)
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "ether ") {
				mac := strings.TrimSpace(strings.TrimPrefix(line, "ether"))
				if idx := strings.IndexByte(mac, ' '); idx > 0 {
					mac = mac[:idx]
				}
				if mac != "" && mac != "02:00:00:00:00:00" {
					return mac
				}
			}
		}
		// 2) Fallback cuando ifconfig está censurado: networksetup NO
		// está capado por TCC y devuelve la MAC burned-in del hardware.
		// NO es la MAC rotada por Private WiFi — pero en mini Private WiFi
		// está desactivado (desktop headless) así que coinciden. Para los
		// nodos con rotación activa es un dato peor que ifconfig pero
		// mejor que nada: identifica el fabricante y la familia del
		// adaptador, y suele seguir asociando dentro del mismo rango OUI.
		// Formato de salida: "Ethernet Address: 2c:ca:16:26:15:7d (Device: en0)"
		// o "Ethernet Address: (null) (Device: en0)" si no hay link.
		out = run("/usr/sbin/networksetup", "-getmacaddress", iface)
		if i := strings.Index(out, "Ethernet Address: "); i >= 0 {
			resto := strings.TrimSpace(out[i+len("Ethernet Address: "):])
			if campos := strings.Fields(resto); len(campos) > 0 && campos[0] != "(null)" {
				return campos[0]
			}
		}
	case "linux":
		// /sys/class/net/<iface>/address — una línea con la MAC. Siempre
		// está en kernel moderno (incluido Android), no exec = no seccomp.
		b, err := os.ReadFile("/sys/class/net/" + iface + "/address")
		if err == nil {
			mac := strings.TrimSpace(string(b))
			if mac != "" && mac != "00:00:00:00:00:00" {
				return mac
			}
		}
	case "windows":
		// Windows SÍ devuelve la MAC real por net.InterfaceByName. Si tenemos
		// iface default la usamos directo; si no (defaultIface() aún no está
		// implementado para windows), escaneamos: primera NIC UP, no-loopback,
		// con HardwareAddr. Suele ser el adaptador wifi o ethernet real.
		if iface != "" {
			if nic, err := net.InterfaceByName(iface); err == nil {
				if mac := nic.HardwareAddr.String(); mac != "" {
					return mac
				}
			}
		}
		ifaces, err := net.Interfaces()
		if err == nil {
			for _, nic := range ifaces {
				if nic.Flags&net.FlagUp == 0 || nic.Flags&net.FlagLoopback != 0 {
					continue
				}
				// MAC real = 6 bytes (EUI-48). Teredo, isatap, ipv6-tunnel y
				// amigos tienen HardwareAddr de 20 bytes o 0: se descartan.
				if len(nic.HardwareAddr) != 6 {
					continue
				}
				// Nombres típicos de interfaces virtuales en Windows que
				// NO son la NIC física que un vecino de LAN vería: hyper-v,
				// WSL, VPN, Tailscale, bluetooth PAN. Nos interesa la
				// ethernet/wifi del hardware.
				low := strings.ToLower(nic.Name)
				if strings.Contains(low, "vethernet") ||
					strings.Contains(low, "pseudo") ||
					strings.Contains(low, "teredo") ||
					strings.Contains(low, "isatap") ||
					strings.Contains(low, "tailscale") ||
					strings.Contains(low, "bluetooth") ||
					strings.Contains(low, "vmware") ||
					strings.Contains(low, "virtualbox") ||
					strings.Contains(low, "loopback") {
					continue
				}
				if mac := nic.HardwareAddr.String(); mac != "" {
					return mac
				}
			}
		}
	}
	return ""
}

// findTailscale localiza el binario de tailscale sin pasar por PATH en Android
// (ver gotcha de seccomp en specs_linux.go). Devuelve "" si no existe.
func findTailscale() string {
	candidatos := []string{}
	switch runtime.GOOS {
	case "darwin":
		candidatos = append(candidatos,
			"/Applications/Tailscale.app/Contents/MacOS/Tailscale",
			"/opt/homebrew/bin/tailscale",
			"/usr/local/bin/tailscale",
		)
	case "linux":
		candidatos = append(candidatos,
			"/usr/bin/tailscale",
			"/usr/local/bin/tailscale",
		)
	case "windows":
		candidatos = append(candidatos,
			`C:\Program Files\Tailscale\tailscale.exe`,
			`C:\Program Files (x86)\Tailscale\tailscale.exe`,
		)
	}
	for _, p := range candidatos {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

// findLimactl localiza limactl. Solo Mac en la práctica (mini: ~/.local/lima,
// lia: ~/.local/lima). Devuelve "" si no existe (todos los demás nodos).
func findLimactl() string {
	candidatos := []string{}
	if home := os.Getenv("HOME"); home != "" {
		candidatos = append(candidatos, filepath.Join(home, ".local/lima/bin/limactl"))
	}
	// Si nodemesh corre como root (LaunchDaemon), HOME puede estar vacío o
	// apuntar a /var/root; probamos las rutas de los usuarios conocidos.
	candidatos = append(candidatos,
		"/Users/steven.sierra/.local/lima/bin/limactl", // mini
		"/Users/steven/.local/lima/bin/limactl",        // lia
		"/opt/homebrew/bin/limactl",
		"/usr/local/bin/limactl",
	)
	for _, p := range candidatos {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

//go:build darwin || linux

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// limactlList ejecuta `limactl list -f ...` cambiando UID al dueño del binario
// cuando corremos como root. Devuelve "nombre:estado,…" o "" si falla o no
// hay VMs. Separado de supervision.go porque usa syscall.Credential que no
// existe en Windows. Ver limaVMs() para el porqué del UID switch.
func limactlList(bin string) string {
	// Dos layouts conocidos:
	//   mini:      ~/.local/lima/bin/limactl  → HOME = 4 niveles arriba.
	//   lia/brew:  /opt/homebrew/bin/limactl  → HOME NO sale del path.
	// En el segundo caso buscamos un /Users/<u>/.lima/<vm> real y usamos ese
	// Users/<u> como HOME. Si no hay nada, salimos sin ejecutar.
	home := ""
	if strings.HasSuffix(bin, "/.local/lima/bin/limactl") {
		home = filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(bin))))
	} else {
		home = findLimaHome()
		if home == "" {
			return ""
		}
	}
	var ownerUID, ownerGID uint32
	// Dueño del HOME, no del binario: brew-installed limactl es de root/admin,
	// y lo que necesitamos es el UID que puede leer ~/.lima.
	if fi, err := os.Stat(home); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			ownerUID, ownerGID = st.Uid, st.Gid
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, bin, "list", "-f", "{{.Name}}:{{.Status}}")
	// Filtra HOME del env padre: libc en macOS/Linux lee la PRIMERA
	// ocurrencia, así que un append al final no sobreescribe.
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "HOME=") {
			env = append(env, kv)
		}
	}
	env = append(env, "HOME="+home)
	c.Env = env
	// Cambia UID del child solo si somos root y el dueño es otro.
	if os.Geteuid() == 0 && ownerUID != 0 && ownerUID != 0xFFFFFFFF {
		c.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{Uid: ownerUID, Gid: ownerGID},
		}
	}
	out, err := c.Output()
	if err != nil {
		return ""
	}
	var vms []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l := strings.TrimSpace(line); l != "" {
			vms = append(vms, l)
		}
	}
	return strings.Join(vms, ",")
}

// findLimaHome enumera /Users y devuelve el primer home que tenga al menos
// una VM Lima real (un subdir de .lima/ que NO sea _config). Vacío si no hay
// ninguno. Pensado para hosts donde limactl se instaló por brew en
// /opt/homebrew/bin/ y las VMs viven en el home del usuario, no en el de root.
func findLimaHome() string {
	entries, err := os.ReadDir("/Users")
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || e.Name() == "Shared" {
			continue
		}
		limaDir := filepath.Join("/Users", e.Name(), ".lima")
		vms, err := os.ReadDir(limaDir)
		if err != nil {
			continue
		}
		for _, v := range vms {
			if v.IsDir() && v.Name() != "_config" {
				return filepath.Join("/Users", e.Name())
			}
		}
	}
	return ""
}

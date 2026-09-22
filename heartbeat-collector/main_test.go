package main

import (
	"strings"
	"testing"
	"time"
)

// Un nodo movil que se va no es un incidente. Sin esta exencion, el dead-man
// alertaba por un portatil cerrado o una tablet guardada, que es justo el ruido
// que hace que se dejen de leer las alertas que si importan.
func TestEsMovilSoloExentaALosDeLaLista(t *testing.T) {
	cfg.Moviles = []string{"macbook-pro", "legion", "rabbit-r1", "rigby"}

	for _, n := range cfg.Moviles {
		if !esMovil(n) {
			t.Errorf("%s esta en la lista de moviles y deberia estar exento", n)
		}
	}
	// Los fijos tienen que seguir alertando: son los que sostienen el cluster.
	for _, n := range []string{"entry", "athena", "gcp-east", "x1-nano", "steven-mini", "dell-xps"} {
		if esMovil(n) {
			t.Errorf("%s es un nodo fijo: que se vaya SI es un incidente", n)
		}
	}
}

func TestSinListaNadieQuedaExento(t *testing.T) {
	cfg.Moviles = nil
	if esMovil("macbook-pro") {
		t.Error("sin lista configurada no debe eximir a nadie")
	}
}

// MCL-199: el mensaje del dead-man tiene que traer memoria cuando la hay, y
// no inventar un "0/0 MB (0%)" cuando un cliente viejo (o un nodo movil que
// nunca la mando) no la trajo nunca.
func TestMemInfoSuffixConDatos(t *testing.T) {
	ns := &NodeState{MemUsedMB: 3502, MemTotalMB: 4096}
	got := memInfoSuffix(ns)
	want := " Última memoria conocida: 3502/4096 MB (85%)."
	if got != want {
		t.Errorf("memInfoSuffix() = %q, want %q", got, want)
	}
}

func TestMemInfoSuffixSinDatos(t *testing.T) {
	ns := &NodeState{}
	if got := memInfoSuffix(ns); got != "" {
		t.Errorf("memInfoSuffix() sin memoria = %q, want vacio", got)
	}
}

// Una parada anunciada silencia el silencio de ESE nodo y de ninguno mas, y se
// acaba sola. Lo contrario —silenciar "hasta nuevo aviso"— es apagar el
// vigilante y olvidarse de encenderlo, que es como se llega a no enterarse de
// una caida (2026-09-18).
func TestMantenimientoSoloCallaAlNodoAnunciadoYCaduca(t *testing.T) {
	ahora := time.Now().Unix()
	state.Mantenimiento = map[string]int64{
		"entry":   ahora + 600, // en curso
		"x1-nano": ahora - 60,  // ya vencido
	}
	if !enMantenimiento("entry") {
		t.Error("entry esta en mantenimiento en curso: no debe alertar por su silencio")
	}
	if enMantenimiento("x1-nano") {
		t.Error("el mantenimiento de x1-nano ya vencio: tiene que volver a vigilarse solo")
	}
	if enMantenimiento("steven-mini") {
		t.Error("un nodo que nadie anuncio no puede quedar silenciado de rebote")
	}
	state.Mantenimiento = nil
	if enMantenimiento("entry") {
		t.Error("sin mantenimientos declarados no debe callarse nada")
	}
}

// El fallo de un canal se cuenta por el OTRO, se anuncia una vez, no se repite
// antes de realertHours y la recuperacion SIEMPRE se avisa con cuanto duro.
// Es el mismo criterio del resto de la flota ("si ya va mas de 10 horas ya se
// que esta pasando, lo unico que me interesa es saber si regresa").
func TestRevisarCanalAvisaUnaVezYAlVolver(t *testing.T) {
	state.Canales = map[string]*EstadoCanal{}
	cfg.RealertHours = 8
	var dichos []string
	decir := func(s string) { dichos = append(dichos, s) }
	t0 := time.Now().Unix()

	revisarCanal("telegram", Salud{OK: true}, t0, decir) // sano: calla
	if len(dichos) != 0 {
		t.Fatalf("un canal sano no debe decir nada, dijo %v", dichos)
	}

	roto := Salud{Motivo: "no hay token configurado", Firme: true}
	revisarCanal("telegram", roto, t0+60, decir)
	if len(dichos) != 1 || !strings.Contains(dichos[0], "NO puede avisar") {
		t.Fatalf("deberia denunciar el canal roto, dijo %v", dichos)
	}

	revisarCanal("telegram", roto, t0+120, decir)
	if len(dichos) != 1 {
		t.Fatalf("no debe repetirse antes de realertHours, dijo %v", dichos)
	}

	// Pasadas las 8 h insiste una sola vez mas.
	revisarCanal("telegram", roto, t0+60+8*3600, decir)
	if len(dichos) != 2 || !strings.Contains(dichos[1], "sigue sin poder avisar") {
		t.Fatalf("pasadas las 8h deberia insistir, dijo %v", dichos)
	}

	revisarCanal("telegram", Salud{OK: true}, t0+60+9*3600, decir)
	if len(dichos) != 3 || !strings.Contains(dichos[2], "✅") || !strings.Contains(dichos[2], "min sin poder avisar") {
		t.Fatalf("la recuperacion se avisa con la duracion, dijo %v", dichos)
	}
}

// El caso que de verdad importa: un token vacio tiene que DETECTARSE, no
// tragarse. Antes, telegram() volvia sin decir nada y el colector llevaba una
// semana sin avisar de nada.
func TestSaludTelegramCazaElTokenVacio(t *testing.T) {
	cfg.TelegramToken, cfg.TelegramChat = "", "66513789"
	s := saludTelegram()
	if s.OK || !strings.Contains(s.Motivo, "telegramToken vacio") {
		t.Errorf("un token vacio tiene que ser un canal roto, dio ok=%v motivo=%q", s.OK, s.Motivo)
	}
	if !s.Firme {
		t.Error("un token vacio no es un bache de red: tiene que denunciarse al primer sondeo")
	}
}

// El ruido del 2026-09-18: cuatro baches de UN solo sondeo, cada uno con su
// "🔴 no puede avisar" y su "✅ vuelve a funcionar" cinco minutos despues. Ocho
// mensajes por nada, y la cola del R1 tapada veinte minutos.
func TestUnBacheDeRedNoSueltaNiUnMensaje(t *testing.T) {
	state.Canales = map[string]*EstadoCanal{}
	cfg.RealertHours = 8
	cfg.CanalesFallosSeguidos = 0 // el de por defecto: 3
	var dichos []string
	decir := func(s string) { dichos = append(dichos, s) }
	t0 := time.Now().Unix()

	bache := Salud{Motivo: "no se alcanza la API: context deadline exceeded"}
	revisarCanal("telegram", bache, t0, decir)
	revisarCanal("telegram", Salud{OK: true}, t0+300, decir)
	revisarCanal("telegram", bache, t0+1800, decir)
	revisarCanal("telegram", Salud{OK: true}, t0+2100, decir)

	if len(dichos) != 0 {
		t.Fatalf("un bache suelto no puede generar mensajes, solto %v", dichos)
	}
	if est := state.Canales["telegram"]; est == nil || !est.OK || est.Fallos != 0 {
		t.Fatalf("el canal deberia seguir sano y sin fallos acumulados: %+v", est)
	}
}

// Y lo contrario: una averia de verdad SI tiene que salir, solo que a los tres
// sondeos en vez de al primero.
func TestTresSondeosMalosSeguidosSiDenuncian(t *testing.T) {
	state.Canales = map[string]*EstadoCanal{}
	cfg.RealertHours = 8
	cfg.CanalesFallosSeguidos = 0
	var dichos []string
	decir := func(s string) { dichos = append(dichos, s) }
	t0 := time.Now().Unix()

	bache := Salud{Motivo: "no se alcanza la API: context deadline exceeded"}
	revisarCanal("telegram", bache, t0, decir)
	revisarCanal("telegram", bache, t0+300, decir)
	if len(dichos) != 0 {
		t.Fatalf("con dos todavia no, solto %v", dichos)
	}
	revisarCanal("telegram", bache, t0+600, decir)
	if len(dichos) != 1 || !strings.Contains(dichos[0], "3 sondeos seguidos") {
		t.Fatalf("al tercero deberia denunciarlo diciendo cuantos van, dijo %v", dichos)
	}

	// Y al volver, la recuperacion de siempre.
	revisarCanal("telegram", Salud{OK: true}, t0+900, decir)
	if len(dichos) != 2 || !strings.Contains(dichos[1], "✅") {
		t.Fatalf("la recuperacion se avisa, dijo %v", dichos)
	}
}

// Un 401 no es un bache: la API contesto y dijo que el token no vale.
func TestElFallo401SeDenunciaAlPrimerSondeo(t *testing.T) {
	state.Canales = map[string]*EstadoCanal{}
	cfg.RealertHours = 8
	var dichos []string
	decir := func(s string) { dichos = append(dichos, s) }

	revisarCanal("telegram", Salud{Motivo: "Unauthorized", Firme: true}, time.Now().Unix(), decir)
	if len(dichos) != 1 || strings.Contains(dichos[0], "sondeos seguidos") {
		t.Fatalf("un fallo firme sale ya y sin contar sondeos, dijo %v", dichos)
	}
}

func TestURLSaludNotifySeDerivaDeLaDeEnvio(t *testing.T) {
	cfg.NotifyURL = "http://127.0.0.1:8090/v1/messages"
	if got := urlSaludNotify(); got != "http://127.0.0.1:8090/v1/health" {
		t.Errorf("urlSaludNotify() = %q", got)
	}
	cfg.NotifyURL = ""
	if got := urlSaludNotify(); got != "" {
		t.Errorf("sin notifyURL no hay salud que consultar, dio %q", got)
	}
}

// Un log no puede publicar el token. Go mete la URL entera en sus errores de
// http, y la URL de Telegram lleva el token dentro.
func TestSinTokenNoDejaEscaparElToken(t *testing.T) {
	cfg.TelegramToken = "123456:ABCdefGHIjklMNO"
	err := `Get "https://api.telegram.org/bot123456:ABCdefGHIjklMNO/getMe": context deadline exceeded`
	got := sinToken(err)
	if strings.Contains(got, cfg.TelegramToken) {
		t.Fatalf("el token sigue en el texto: %q", got)
	}
	if !strings.Contains(got, "<token>") {
		t.Errorf("deberia quedar la marca del token tapado: %q", got)
	}
	cfg.TelegramToken = ""
	if got := sinToken("sin token no hay nada que tapar"); got != "sin token no hay nada que tapar" {
		t.Errorf("sin token configurado no debe tocar el texto: %q", got)
	}
}

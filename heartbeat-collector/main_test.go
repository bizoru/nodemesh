package main

import (
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

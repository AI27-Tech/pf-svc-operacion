package adjuntos

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestAlmacenStubFirmaURLDeterministaYExpira: el stub devuelve una URL de
// marcador que incluye viaje y adjunto, distingue subida de descarga, y expira
// según la vida configurada a partir del reloj inyectado. Corre sin Postgres.
func TestAlmacenStubFirmaURLDeterministaYExpira(t *testing.T) {
	ancla := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	a := AlmacenStub{Base: "https://almacen.test", Vida: 10 * time.Minute, Ahora: func() time.Time { return ancla }}
	ctx := context.Background()

	subida, expS, err := a.URLSubida(ctx, "viaje-1", "adj-1", "application/pdf")
	if err != nil {
		t.Fatalf("URLSubida: %v", err)
	}
	descarga, expD, err := a.URLDescarga(ctx, "viaje-1", "adj-1")
	if err != nil {
		t.Fatalf("URLDescarga: %v", err)
	}

	for _, c := range []struct{ nombre, url, op string }{
		{"subida", subida, "op=subida"},
		{"descarga", descarga, "op=descarga"},
	} {
		if !strings.HasPrefix(c.url, "https://almacen.test/viajes/viaje-1/adjuntos/adj-1") || !strings.Contains(c.url, c.op) {
			t.Errorf("%s = %q, se esperaba prefijo con viaje/adjunto y %s", c.nombre, c.url, c.op)
		}
	}
	if subida == descarga {
		t.Error("la URL de subida y la de descarga no deben coincidir")
	}
	want := ancla.Add(10 * time.Minute)
	if !expS.Equal(want) || !expD.Equal(want) {
		t.Errorf("expiración = (%v, %v), se esperaba %v", expS, expD, want)
	}
}

// TestAlmacenStubDefaults: sin Base ni Vida ni reloj, usa el host y la vida por
// omisión y una expiración futura.
func TestAlmacenStubDefaults(t *testing.T) {
	a := AlmacenStub{}
	url, exp, err := a.URLSubida(context.Background(), "v", "a", "")
	if err != nil {
		t.Fatalf("URLSubida: %v", err)
	}
	if !strings.HasPrefix(url, "https://almacen.local/") {
		t.Errorf("url = %q, se esperaba el host por omisión", url)
	}
	if !exp.After(time.Now()) {
		t.Errorf("expiración = %v, se esperaba futura", exp)
	}
}

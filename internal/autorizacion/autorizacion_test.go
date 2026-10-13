package autorizacion

import (
	"context"
	"errors"
	"testing"
	"time"
)

// --- dobles ---

type frescuraFija struct{ t time.Time }

func (f frescuraFija) UltimaSenal() time.Time { return f.t }

type autoridadFalsa struct {
	revocada bool
	err      error
	llamada  bool
}

func (a *autoridadFalsa) SesionRevocada(ctx context.Context, sesionID string) (bool, error) {
	a.llamada = true
	return a.revocada, a.err
}

// --- Revocada: los tres caminos de ADR 26 §3/§5 ---

func TestRevocadaRespuestaLocalGana(t *testing.T) {
	p := NuevaProyeccion()
	p.Marcar("s-1")
	if !p.Revocada(context.Background(), "s-1") {
		t.Error("una sesión en el set local debe rechazarse")
	}
	// Marcar es idempotente: dos veces no cambia el resultado.
	p.Marcar("s-1")
	if !p.Revocada(context.Background(), "s-1") {
		t.Error("Marcar idempotente: la sesión sigue revocada")
	}
}

func TestRevocadaSinFrescuraRutaLocal(t *testing.T) {
	p := NuevaProyeccion()
	// Sin ConFrescura: una sesión no marcada se considera viva (ruta local).
	if p.Revocada(context.Background(), "s-desconocida") {
		t.Error("sin frescura configurada, una sesión no marcada debe pasar (viva)")
	}
}

func TestRevocadaCanalFrescoNoConsultaAutoridad(t *testing.T) {
	p := NuevaProyeccion()
	aut := &autoridadFalsa{revocada: true} // si se consultara, rechazaría
	// Última señal AHORA → canal fresco (umbral 15s).
	p.ConFrescura(time.Now, frescuraFija{t: time.Now()}, aut, 15*time.Second)

	if p.Revocada(context.Background(), "s-viva") {
		t.Error("canal fresco: una sesión no marcada debe pasar sin consultar identidad")
	}
	if aut.llamada {
		t.Error("canal fresco: NO debe consultarse la autoridad (fallback)")
	}
}

func TestRevocadaCanalObsoletoCaeAlFallback(t *testing.T) {
	ahora := time.Now()
	// Última señal hace 20s, umbral 15s → canal obsoleto.
	vieja := ahora.Add(-20 * time.Second)

	// Caso A: identidad confirma vigencia → pasa.
	p := NuevaProyeccion()
	autViva := &autoridadFalsa{revocada: false}
	p.ConFrescura(func() time.Time { return ahora }, frescuraFija{t: vieja}, autViva, 15*time.Second)
	if p.Revocada(context.Background(), "s-x") {
		t.Error("canal obsoleto + identidad confirma vigencia → debe pasar")
	}
	if !autViva.llamada {
		t.Error("canal obsoleto: debe consultarse la autoridad")
	}

	// Caso B: identidad confirma revocación → rechaza.
	p2 := NuevaProyeccion()
	p2.ConFrescura(func() time.Time { return ahora }, frescuraFija{t: vieja}, &autoridadFalsa{revocada: true}, 15*time.Second)
	if !p2.Revocada(context.Background(), "s-y") {
		t.Error("canal obsoleto + identidad confirma revocación → debe rechazar")
	}

	// Caso C: identidad NO responde → fail-closed (rechaza).
	p3 := NuevaProyeccion()
	p3.ConFrescura(func() time.Time { return ahora }, frescuraFija{t: vieja}, &autoridadFalsa{err: errors.New("timeout")}, 15*time.Second)
	if !p3.Revocada(context.Background(), "s-z") {
		t.Error("canal obsoleto + identidad no responde → FAIL-CLOSED (rechazar)")
	}
}

func TestRevocadaNilEsSegura(t *testing.T) {
	var p *Proyeccion
	if p.Revocada(context.Background(), "s") {
		t.Error("una proyección nil no debe rechazar (no hay tombstones)")
	}
}

// --- SenalCanal ---

func TestSenalCanalMarcar(t *testing.T) {
	base := time.Now()
	reloj := base
	s := NuevaSenalCanal(func() time.Time { return reloj })
	if !s.UltimaSenal().Equal(base) {
		t.Error("una señal nueva arranca fresca en el instante de creación")
	}
	reloj = base.Add(10 * time.Second)
	s.Marcar()
	if !s.UltimaSenal().Equal(reloj) {
		t.Error("Marcar debe registrar el instante actual")
	}
}

// --- Autoridad sobre el puerto Revocacion ---

type consultaFalsa struct {
	revocada bool
	err      error
}

func (c consultaFalsa) ObtenerSesion(ctx context.Context, sesionID string) (bool, error) {
	return c.revocada, c.err
}

func TestNuevaAutoridadDelegaEnObtenerSesion(t *testing.T) {
	aut := NuevaAutoridad(consultaFalsa{revocada: true})
	rev, err := aut.SesionRevocada(context.Background(), "s-1")
	if err != nil || !rev {
		t.Errorf("la autoridad debe delegar en ObtenerSesion: got (%v,%v)", rev, err)
	}
}

// --- reconstructor: readiness y precarga ---

// workerFalso es un worker.Worker controlable: Run bloquea hasta cancelar.
type workerFalso struct{ listo error }

func (w *workerFalso) Name() string                    { return "sub-falso" }
func (w *workerFalso) Ready(ctx context.Context) error { return w.listo }
func (w *workerFalso) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

type snapshotFalso struct {
	ids []string
	err error
}

func (s snapshotFalso) ListarRevocadas(ctx context.Context) ([]string, error) {
	return s.ids, s.err
}

func TestReconstructorPrecargaYConverge(t *testing.T) {
	proy := NuevaProyeccion()
	rec := NuevoReconstructor(&workerFalso{}, snapshotFalso{ids: []string{"s-1", "s-2"}}, proy)

	// Antes de arrancar: not-ready.
	if rec.Ready(context.Background()) == nil {
		t.Error("antes de converger, Ready debe ser not-ready")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = rec.Run(ctx); close(done) }()

	// Esperar a que converja (precarga rápida con doble en memoria).
	deadline := time.After(2 * time.Second)
	for {
		if rec.Ready(context.Background()) == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("el reconstructor no convergió a ready a tiempo")
		case <-time.After(5 * time.Millisecond):
		}
	}

	// Convergido: las revocadas del snapshot quedaron en la proyección.
	if !proy.Revocada(context.Background(), "s-1") || !proy.Revocada(context.Background(), "s-2") {
		t.Error("tras converger, las revocadas del snapshot deben estar marcadas")
	}
	cancel()
	<-done
}

func TestReconstructorPrecargaFallaQuedaNotReady(t *testing.T) {
	rec := NuevoReconstructor(&workerFalso{}, snapshotFalso{err: errors.New("identidad caída")}, NuevaProyeccion())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = rec.Run(ctx); close(done) }()

	// Darle un momento para intentar la precarga y fallar.
	time.Sleep(50 * time.Millisecond)
	err := rec.Ready(context.Background())
	if err == nil {
		t.Error("si la precarga falla, Ready debe seguir not-ready")
	}
	cancel()
	<-done
}

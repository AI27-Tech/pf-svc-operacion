package catalogoscache

import (
	"context"
	"errors"
	"testing"
	"time"

	catalogos "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/catalogos"
	"github.com/google/uuid"
)

// subyacenteFake cuenta cuántas veces se consulta cada operación y permite
// forzar un error (catalogos caído). Es el "catalogos" detrás de la caché.
type subyacenteFake struct {
	llamadasVehiculo int
	llamadasCustodio int
	err              error // si !=nil, ambas operaciones devuelven este error
}

func (f *subyacenteFake) AptitudVehiculo(ctx context.Context, vehiculoID, clienteID catalogos.Id) (*catalogos.Aptitud, error) {
	f.llamadasVehiculo++
	if f.err != nil {
		return nil, f.err
	}
	return &catalogos.Aptitud{Apto: true, RecursoId: vehiculoID}, nil
}

func (f *subyacenteFake) AptitudCustodio(ctx context.Context, custodioID, clienteID catalogos.Id) (*catalogos.Aptitud, error) {
	f.llamadasCustodio++
	if f.err != nil {
		return nil, f.err
	}
	return &catalogos.Aptitud{Apto: true, RecursoId: custodioID}, nil
}

// relojManual es un reloj controlable para probar el vencimiento del TTL sin
// dormir en el test.
type relojManual struct{ t time.Time }

func (r *relojManual) ahora() time.Time { return r.t }
func (r *relojManual) avanzar(d time.Duration) {
	r.t = r.t.Add(d)
}

// TestHitDentroDelTTLNoReconsulta: una segunda lectura dentro del TTL se sirve de
// caché y NO vuelve a pegar a catalogos. Es la mitigación N+1 que sostiene la
// caché del consumidor.
func TestHitDentroDelTTLNoReconsulta(t *testing.T) {
	sub := &subyacenteFake{}
	rel := &relojManual{t: time.Unix(0, 0)}
	c := NuevaConReloj(sub, time.Minute, rel.ahora)

	vehID := uuid.New()
	cliID := uuid.New()

	if _, err := c.AptitudVehiculo(context.Background(), vehID, cliID); err != nil {
		t.Fatalf("primera lectura: %v", err)
	}
	rel.avanzar(30 * time.Second) // dentro del TTL de 1 min
	if _, err := c.AptitudVehiculo(context.Background(), vehID, cliID); err != nil {
		t.Fatalf("segunda lectura: %v", err)
	}
	if sub.llamadasVehiculo != 1 {
		t.Errorf("se esperaba 1 consulta al subyacente (segunda servida de caché); hubo %d", sub.llamadasVehiculo)
	}
}

// TestTTLComoTechoDeObsolescencia: pasado el TTL, la entrada vence y la siguiente
// lectura reconsulta a catalogos aunque NUNCA haya llegado un evento de
// invalidación. Es el criterio "broker caído => obsolescencia acotada por TTL".
func TestTTLComoTechoDeObsolescencia(t *testing.T) {
	sub := &subyacenteFake{}
	rel := &relojManual{t: time.Unix(0, 0)}
	c := NuevaConReloj(sub, time.Minute, rel.ahora)

	vehID := uuid.New()
	cliID := uuid.New()

	if _, err := c.AptitudVehiculo(context.Background(), vehID, cliID); err != nil {
		t.Fatalf("primera lectura: %v", err)
	}
	rel.avanzar(90 * time.Second) // más allá del TTL, sin evento de invalidación
	if _, err := c.AptitudVehiculo(context.Background(), vehID, cliID); err != nil {
		t.Fatalf("segunda lectura: %v", err)
	}
	if sub.llamadasVehiculo != 2 {
		t.Errorf("vencido el TTL, se esperaba reconsulta (2); hubo %d", sub.llamadasVehiculo)
	}
}

// TestErrorNoSeCacheaYSePropaga: con catalogos caído, la caché propaga el error
// (degradado) y NO lo cachea. El caso de uso lo traduce a "aptitud desconocida" y
// la asignación procede (DEC-055): la caché nunca convierte el fallo en rechazo
// ni en un apto inventado. Que no se cachee se comprueba porque, restaurado
// catalogos, la siguiente lectura vuelve a consultarlo.
func TestErrorNoSeCacheaYSePropaga(t *testing.T) {
	fallo := errors.New("catalogos caído")
	sub := &subyacenteFake{err: fallo}
	rel := &relojManual{t: time.Unix(0, 0)}
	c := NuevaConReloj(sub, time.Minute, rel.ahora)

	vehID := uuid.New()
	cliID := uuid.New()

	_, err := c.AptitudVehiculo(context.Background(), vehID, cliID)
	if !errors.Is(err, fallo) {
		t.Fatalf("se esperaba que el error del subyacente se propagara; got %v", err)
	}

	// catalogos se restablece: la siguiente lectura DEBE reconsultar (el fallo no
	// quedó cacheado) y ahora sí tener éxito.
	sub.err = nil
	apt, err := c.AptitudVehiculo(context.Background(), vehID, cliID)
	if err != nil {
		t.Fatalf("tras restablecer catalogos: %v", err)
	}
	if apt == nil || !apt.Apto {
		t.Fatalf("se esperaba aptitud apta tras restablecer; got %+v", apt)
	}
	if sub.llamadasVehiculo != 2 {
		t.Errorf("el fallo no debía cachearse; se esperaban 2 consultas, hubo %d", sub.llamadasVehiculo)
	}
}

// TestInvalidarForzaReconsulta: tras Invalidar, la siguiente lectura va a
// catalogos aunque el TTL no haya vencido. Es la invalidación por evento.
func TestInvalidarForzaReconsulta(t *testing.T) {
	sub := &subyacenteFake{}
	rel := &relojManual{t: time.Unix(0, 0)}
	c := NuevaConReloj(sub, time.Hour, rel.ahora) // TTL largo: solo invalida el evento

	vehID := uuid.New()
	cliID := uuid.New()

	if _, err := c.AptitudVehiculo(context.Background(), vehID, cliID); err != nil {
		t.Fatalf("primera lectura: %v", err)
	}
	c.Invalidar(EntidadVehiculo, vehID)
	if _, err := c.AptitudVehiculo(context.Background(), vehID, cliID); err != nil {
		t.Fatalf("segunda lectura: %v", err)
	}
	if sub.llamadasVehiculo != 2 {
		t.Errorf("tras Invalidar se esperaba reconsulta (2); hubo %d", sub.llamadasVehiculo)
	}
}

// TestInvalidarEsIdempotente: invalidar una entidad ausente, o dos veces, no
// explota ni tiene efecto adicional (P13). Tras una sola lectura+invalidación
// repetida, la relectura reconsulta exactamente una vez más.
func TestInvalidarEsIdempotente(t *testing.T) {
	sub := &subyacenteFake{}
	rel := &relojManual{t: time.Unix(0, 0)}
	c := NuevaConReloj(sub, time.Hour, rel.ahora)

	vehID := uuid.New()
	cliID := uuid.New()

	// Invalidar algo que no está cacheado: no-op.
	c.Invalidar(EntidadVehiculo, vehID)

	if _, err := c.AptitudVehiculo(context.Background(), vehID, cliID); err != nil {
		t.Fatalf("primera lectura: %v", err)
	}
	// Doble invalidación de la misma clave.
	c.Invalidar(EntidadVehiculo, vehID)
	c.Invalidar(EntidadVehiculo, vehID)

	if _, err := c.AptitudVehiculo(context.Background(), vehID, cliID); err != nil {
		t.Fatalf("segunda lectura: %v", err)
	}
	if sub.llamadasVehiculo != 2 {
		t.Errorf("doble invalidación debe equivaler a una; se esperaban 2 consultas, hubo %d", sub.llamadasVehiculo)
	}
}

// TestInvalidarNoAfectaOtraEntidad: invalidar un vehículo no tira la entrada de
// un custodio (ni de otro vehículo). La clave de caché distingue por entidad.
func TestInvalidarNoAfectaOtraEntidad(t *testing.T) {
	sub := &subyacenteFake{}
	rel := &relojManual{t: time.Unix(0, 0)}
	c := NuevaConReloj(sub, time.Hour, rel.ahora)

	vehID := uuid.New()
	cstID := uuid.New()
	cliID := uuid.New()

	if _, err := c.AptitudVehiculo(context.Background(), vehID, cliID); err != nil {
		t.Fatalf("lectura vehículo: %v", err)
	}
	if _, err := c.AptitudCustodio(context.Background(), cstID, cliID); err != nil {
		t.Fatalf("lectura custodio: %v", err)
	}

	// Invalidar el vehículo no debe tocar al custodio.
	c.Invalidar(EntidadVehiculo, vehID)

	if _, err := c.AptitudCustodio(context.Background(), cstID, cliID); err != nil {
		t.Fatalf("relectura custodio: %v", err)
	}
	if sub.llamadasCustodio != 1 {
		t.Errorf("la invalidación del vehículo no debía afectar al custodio; consultas custodio=%d", sub.llamadasCustodio)
	}
}

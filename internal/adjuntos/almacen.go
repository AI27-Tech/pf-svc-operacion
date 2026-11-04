package adjuntos

import (
	"context"
	"fmt"
	"time"
)

// AlmacenObjetos firma URL de vida corta para subir y descargar los bytes de un
// adjunto directo contra el almacenamiento de objetos (ADR 32, MinIO). Los bytes
// NUNCA pasan por el servicio. La interfaz la declara este paquete porque es su
// consumidor (regla 2 de 00); el adaptador real vive fuera, cuando ADR 32 aterrice.
//
// La clave del objeto se deriva de (viaje, adjunto): el almacén la alcanza por
// tenant/viaje (mismo patrón que mensajeria). El contrato devuelve la URL y su
// instante de expiración; la vida de la URL la fija el almacén, no es contrato.
type AlmacenObjetos interface {
	// URLSubida firma un PUT de vida corta para subir los bytes del adjunto. Recibe
	// el tipo de medio declarado por si el almacén exige fijar el Content-Type.
	URLSubida(ctx context.Context, viajeID, adjuntoID, tipoMedio string) (url string, expira time.Time, err error)
	// URLDescarga firma un GET de vida corta para descargar los bytes del adjunto.
	URLDescarga(ctx context.Context, viajeID, adjuntoID string) (url string, expira time.Time, err error)
}

// AlmacenStub es el AlmacenObjetos de V1 hasta que ADR 32 fije MinIO y se cablee
// el adaptador real. Devuelve una URL de marcador determinista y una expiración
// corta a partir de un reloj inyectable. No habla con ningún almacenamiento: deja
// el resto del flujo (metadata, auditoría, contrato) completo y probable.
//
// shortcut: URL de marcador, no un almacén real; se reemplaza por el adaptador de
// MinIO cuando ADR 32 esté decidido.
type AlmacenStub struct {
	// Base es el prefijo de la URL de marcador (p. ej. "https://almacen.local").
	Base string
	// Vida es cuánto dura la URL firmada. Cero usa un default de 15 minutos.
	Vida time.Duration
	// Ahora permite anclar el reloj en pruebas. Nil usa time.Now.
	Ahora func() time.Time
}

func (a AlmacenStub) ahora() time.Time {
	if a.Ahora != nil {
		return a.Ahora()
	}
	return time.Now()
}

func (a AlmacenStub) expira() time.Time {
	vida := a.Vida
	if vida <= 0 {
		vida = 15 * time.Minute
	}
	return a.ahora().Add(vida).UTC()
}

func (a AlmacenStub) base() string {
	if a.Base == "" {
		return "https://almacen.local"
	}
	return a.Base
}

// URLSubida devuelve una URL de marcador de subida. tipoMedio se ignora en el
// stub; el adaptador real lo usará para fijar el Content-Type del PUT firmado.
func (a AlmacenStub) URLSubida(_ context.Context, viajeID, adjuntoID, _ string) (string, time.Time, error) {
	return fmt.Sprintf("%s/viajes/%s/adjuntos/%s?op=subida", a.base(), viajeID, adjuntoID), a.expira(), nil
}

// URLDescarga devuelve una URL de marcador de descarga.
func (a AlmacenStub) URLDescarga(_ context.Context, viajeID, adjuntoID string) (string, time.Time, error) {
	return fmt.Sprintf("%s/viajes/%s/adjuntos/%s?op=descarga", a.base(), viajeID, adjuntoID), a.expira(), nil
}

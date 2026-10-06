package operacion

import (
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
)

// errNoImplementado marca un caso de uso aún no implementado en el scaffold.
// Usa la taxonomía de la plataforma (I3) para que la respuesta sea coherente
// entre el adaptador HTTP y el en proceso.
func errNoImplementado(op string) error {
	return platformerrors.NewRoto("operacion: %s no implementado todavía (scaffold)", op)
}

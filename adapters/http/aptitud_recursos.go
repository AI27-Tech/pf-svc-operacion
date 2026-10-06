// Package http contiene los adaptadores HTTP de los puertos de salida de
// operacion (clientes generados de pf-pkg-contracts). El adaptador EN PROCESO
// del mismo puerto NO vive aquí: vive en pf-svc-core, el único repo que ve a
// operacion y a catalogos a la vez (regla 2 de 00, DEC-050 §2). Este adaptador
// lo usa cmd/api (standalone, I5) y cualquier raíz que no co-ubique catalogos.
package http

import (
	"context"
	"fmt"
	"net/http"
	"time"

	catalogos "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/catalogos"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	operacion "github.com/AI27-Tech/pf-svc-operacion"
)

// presupuestoAptitud es el presupuesto de tiempo de la arista a catalogos. El
// adaptador en proceso de core debe usar EXACTAMENTE el mismo valor (I3); por
// eso se exporta junto al adaptador. TODO(operacion): si este valor debe
// configurarse por despliegue, moverlo a Deps/config; hoy es el default común.
const presupuestoAptitud = 2 * time.Second

// AptitudRecursosHTTP implementa operacion.AptitudRecursos con el cliente
// generado de catalogos. Respeta el presupuesto de tiempo y traduce los errores
// a la taxonomía de la plataforma, igual que debe hacer el adaptador en proceso.
type AptitudRecursosHTTP struct {
	cli *catalogos.ClientWithResponses
}

var _ operacion.AptitudRecursos = (*AptitudRecursosHTTP)(nil)

// NewAptitudRecursosHTTP crea el adaptador apuntando a la URL base de catalogos.
func NewAptitudRecursosHTTP(baseURL string, doer catalogos.HttpRequestDoer) (*AptitudRecursosHTTP, error) {
	opts := []catalogos.ClientOption{}
	if doer != nil {
		opts = append(opts, catalogos.WithHTTPClient(doer))
	}
	cli, err := catalogos.NewClientWithResponses(baseURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("operacion: crear cliente de catalogos: %w", err)
	}
	return &AptitudRecursosHTTP{cli: cli}, nil
}

// AptitudUnidad devuelve la aptitud informativa de una unidad-átomo (DEC-055).
func (a *AptitudRecursosHTTP) AptitudUnidad(ctx context.Context, unidadID catalogos.Id) (*catalogos.Aptitud, error) {
	ctx, cancel := context.WithTimeout(ctx, presupuestoAptitud)
	defer cancel()

	resp, err := a.cli.AptitudUnidadWithResponse(ctx, unidadID, &catalogos.AptitudUnidadParams{})
	if err != nil {
		return nil, traducirError("AptitudUnidad", err)
	}
	return aptitudDeRespuesta(resp.StatusCode(), resp.JSON200)
}

// AptitudCustodio devuelve la aptitud informativa de un custodio (DEC-055).
func (a *AptitudRecursosHTTP) AptitudCustodio(ctx context.Context, custodioID catalogos.Id) (*catalogos.Aptitud, error) {
	ctx, cancel := context.WithTimeout(ctx, presupuestoAptitud)
	defer cancel()

	resp, err := a.cli.AptitudCustodioWithResponse(ctx, custodioID, &catalogos.AptitudCustodioParams{})
	if err != nil {
		return nil, traducirError("AptitudCustodio", err)
	}
	return aptitudDeRespuesta(resp.StatusCode(), resp.JSON200)
}

// aptitudDeRespuesta traduce el código HTTP a la taxonomía de la plataforma. Un
// fallo NO gatea: el caso de uso trata el error como "aptitud desconocida"
// (DEC-055); por eso se devuelve degradado, no roto, cuando catalogos no da 200.
func aptitudDeRespuesta(status int, apt *catalogos.Aptitud) (*catalogos.Aptitud, error) {
	switch status {
	case http.StatusOK:
		return apt, nil
	case http.StatusNotFound:
		return nil, platformerrors.NewNoEncontrado("catalogos: recurso no encontrado")
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, platformerrors.NewNoAutorizado("catalogos: no autorizado")
	default:
		// Degradado, no roto: la aptitud es informativa y su ausencia no bloquea.
		return nil, platformerrors.NewDegradado("catalogos: aptitud no disponible (status %d)", status)
	}
}

func traducirError(op string, err error) error {
	// Timeout/cancelación o fallo de red: degradado (la aptitud no gatea).
	return platformerrors.NewDegradadoErr(err, "operacion: %s contra catalogos falló", op)
}

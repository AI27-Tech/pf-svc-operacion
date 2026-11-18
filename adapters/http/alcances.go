package http

import (
	"context"
	"fmt"
	"net/http"

	identidad "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/identidad"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	operacion "github.com/AI27-Tech/pf-svc-operacion"
)

// AlcancesHTTP implementa operacion.Alcances (identidad.listarAlcances, DEC-149
// #20) con el cliente generado de identidad. listarAlcances exige un token de
// servicio válido: el doer que inyecta la raíz lo agrega. El presupuesto de
// tiempo lo pone el llamador (la reconstrucción), igual que en proceso (I3).
type AlcancesHTTP struct {
	cli *identidad.ClientWithResponses
}

var _ operacion.Alcances = (*AlcancesHTTP)(nil)

// NewAlcancesHTTP crea el adaptador apuntando a la URL base de identidad.
func NewAlcancesHTTP(baseURL string, doer identidad.HttpRequestDoer) (*AlcancesHTTP, error) {
	opts := []identidad.ClientOption{}
	if doer != nil {
		opts = append(opts, identidad.WithHTTPClient(doer))
	}
	cli, err := identidad.NewClientWithResponses(baseURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("operacion: crear cliente de identidad: %w", err)
	}
	return &AlcancesHTTP{cli: cli}, nil
}

// ListarAlcances pide el snapshot (sin cursor) o su continuación (con cursor).
func (a *AlcancesHTTP) ListarAlcances(ctx context.Context, params identidad.ListarAlcancesParams) (*identidad.PaginaAlcances, error) {
	resp, err := a.cli.ListarAlcancesWithResponse(ctx, &params)
	if err != nil {
		return nil, platformerrors.NewDegradadoErr(err, "operacion: ListarAlcances contra identidad falló")
	}
	switch resp.StatusCode() {
	case http.StatusOK:
		return resp.JSON200, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, platformerrors.NewNoAutorizado("identidad: no autorizado")
	default:
		return nil, platformerrors.NewDegradado("identidad: respuesta no disponible (status %d)", resp.StatusCode())
	}
}

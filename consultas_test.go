package operacion

import (
	"context"
	"testing"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
)

// Esta prueba fija la FORMA de ConsultasAPI contra la firma que declara el
// consumidor bff (bff.OperacionPort), sin importar su paquete (operacion solo
// tiene a catalogos/identidad como aristas salientes; no depende de bff, que es
// borde). Replica aquí la firma del puerto; si ConsultasAPI deja de satisfacerla
// el test no compila. La conformación contra el puerto REAL la prueba core
// cuando cablea el adaptador en proceso bff → operacion.
type puertoOperacionBFF interface {
	CrearViaje(ctx context.Context, body operclient.CrearViajeEntrada) (*operclient.Viaje, error)
	ListarViajes(ctx context.Context, params operclient.ListarViajesParams) (*operclient.PaginaViajes, error)
	ObtenerViaje(ctx context.Context, id operclient.Id) (*operclient.ViajeDetalle, error)
	ListarBitacora(ctx context.Context, id operclient.Id, params operclient.ListarBitacoraParams) (*operclient.PaginaBitacora, error)
	ConsultarAuditoria(ctx context.Context, id operclient.Id, params operclient.ConsultarAuditoriaParams) (*operclient.PaginaAuditoria, error)
	RegistrarDescargaViaje(ctx context.Context, id operclient.Id) (*operclient.Viaje, error)
	TransicionarViaje(ctx context.Context, id operclient.Id, body operclient.TransicionEntrada) (*operclient.Viaje, error)
	FijarEta(ctx context.Context, id operclient.Id, body operclient.EtaEntrada) (*operclient.Viaje, error)
	AsignarRecursos(ctx context.Context, id operclient.Id, body operclient.AsignacionEntrada) (*operclient.Asignacion, error)
	RegistrarEntradaBitacora(ctx context.Context, id operclient.Id, body operclient.EntradaBitacoraEntrada) (*operclient.EntradaBitacora, error)
	AbrirIncidencia(ctx context.Context, id operclient.Id, body operclient.IncidenciaEntrada) (*operclient.Incidencia, error)
	ResolverIncidencia(ctx context.Context, id, iid operclient.Id, body operclient.ResolucionIncidenciaEntrada) (*operclient.Incidencia, error)
	ListarEstadias(ctx context.Context, id operclient.Id, params operclient.ListarEstadiasParams) (*operclient.PaginaEstadias, error)
	AbrirEstadia(ctx context.Context, id operclient.Id, body operclient.EstadiaEntrada) (*operclient.Estadia, error)
	CerrarEstadia(ctx context.Context, id, eid operclient.Id) (*operclient.Estadia, error)
}

// TestConsultasSatisfaceElPuertoDeBFF comprueba, en tiempo de compilación, que
// la fachada Consultas() tiene exactamente la forma que bff espera.
func TestConsultasSatisfaceElPuertoDeBFF(t *testing.T) {
	var m Module
	var _ puertoOperacionBFF = m.Consultas()
}

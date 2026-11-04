// Package adjuntos implementa la evidencia del viaje (E6-T4 / E6-HU-027): la
// metadata de los adjuntos vive en el esquema `bitacora` (rol op_bitacora); los
// BYTES viven en el almacenamiento de objetos (ADR 32, MinIO) y NUNCA pasan por
// el servicio. La subida y la descarga usan una URL de vida corta que firma el
// AlmacenObjetos. Cada acceso (crear intención de subida, descargar) deja su
// rastro de auditoría en la MISMA transacción vía el interceptor (P3, E1-T7).
//
// Alcance V1 (shortcut documentado):
//   - El almacenamiento real es un puerto stubeado hasta ADR 32 (ver almacen.go).
//     La metadata, la auditoría y el contrato quedan completos; solo la URL
//     firmada es de marcador. shortcut: URL de marcador, hasta que ADR 32 fije
//     MinIO y se cablee el adaptador real.
//   - El escaneo antivirus no existe (pendiente de Seguridad, brief E8-T17): el
//     adjunto nace `pendiente` (lo que dice el contrato) y nada lo mueve. NO se
//     bloquea la descarga por estado_escaneo: una guarda sin escáner que mueva el
//     estado bloquearía todo. shortcut: sin guarda de escaneo, hasta que exista
//     el escáner (DT a registrar).
//   - El alcance por unidad/tenant no se aplica (DT-07, DEC-045/DEC-053
//     pendientes): la autorización es por ROL, igual que el resto de operacion.
package adjuntos

import (
	"context"
	"errors"
	"fmt"
	"strings"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-svc-operacion/internal/auditoria"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// BeginTx es la frontera transaccional (Unit of Work): el caso de uso controla el
// commit, no el repositorio. Mismo patrón que bitacora, viajes y descargas.
type BeginTx interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Acciones de auditoría de la evidencia (E1-T7): valores estables del catálogo
// de acciones, como `descarga_expediente` en descargas.
const (
	accionSubida   = "adjunto_intencion_subida"
	accionDescarga = "adjunto_descarga"
)

// Servicio sirve los adjuntos del viaje. Dos fronteras transaccionales, una por
// rol (I1): `bitacora` (op_bitacora) para la metadata del adjunto, `auditoria`
// (op_auditoria) para el rastro probatorio vía el interceptor. El AlmacenObjetos
// firma las URL de vida corta; los bytes no pasan por aquí.
type Servicio struct {
	bitacora  BeginTx
	auditoria BeginTx
	almacen   AlmacenObjetos
}

// NewServicio recibe el pool del rol op_bitacora (metadata), el del rol
// op_auditoria (rastro) y el almacén de objetos. No lee entorno.
func NewServicio(bitacora, auditoria BeginTx, almacen AlmacenObjetos) *Servicio {
	return &Servicio{bitacora: bitacora, auditoria: auditoria, almacen: almacen}
}

// Listar devuelve los metadatos de los adjuntos del viaje, más recientes
// primero. No devuelve bytes. Sin paginación en V1 (el contrato PaginaAdjuntos
// solo trae items; DT-08).
func (s *Servicio) Listar(ctx context.Context, viajeID operclient.Id) (*operclient.PaginaAdjuntos, error) {
	tx, err := s.bitacora.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx,
		`SELECT id, viaje_id, nombre, tipo_medio, tamano_bytes, checksum_sha256, estado_escaneo, creado_en
		   FROM bitacora.adjunto WHERE viaje_id = $1 ORDER BY creado_en DESC, id DESC`,
		viajeID,
	)
	if err != nil {
		return nil, fmt.Errorf("operacion: listar adjuntos: %w", err)
	}
	defer rows.Close()

	items := []operclient.Adjunto{}
	for rows.Next() {
		var (
			a        operclient.Adjunto
			checksum *string
			escaneo  string
		)
		if err := rows.Scan(&a.Id, &a.ViajeId, &a.Nombre, &a.TipoMedio, &a.TamanoBytes, &checksum, &escaneo, &a.CreadoEn); err != nil {
			return nil, fmt.Errorf("operacion: escanear adjunto: %w", err)
		}
		a.ChecksumSha256 = checksum
		a.EstadoEscaneo = operclient.AdjuntoEstadoEscaneo(escaneo)
		items = append(items, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("operacion: recorrer adjuntos: %w", err)
	}
	return &operclient.PaginaAdjuntos{Items: items}, nil
}

// CrearIntencionSubida inserta la metadata del adjunto (estado_escaneo nace
// `pendiente`), pide al almacén una URL de subida de vida corta y deja el rastro
// de auditoría, todo con exactamente una escritura de metadata por intención. La
// metadata y el rastro van en transacciones de sus roles respectivos; la URL la
// firma el almacén fuera de la base. actorID es el sujeto del token (quién sube).
func (s *Servicio) CrearIntencionSubida(ctx context.Context, viajeID operclient.Id, in operclient.IntencionSubidaAdjuntoEntrada, actorID string) (*operclient.IntencionSubidaAdjunto, error) {
	if strings.TrimSpace(in.Nombre) == "" {
		return nil, platformerrors.NewValidacion("operacion: el nombre del adjunto es obligatorio")
	}
	if in.TamanoBytes <= 0 {
		return nil, platformerrors.NewValidacion("operacion: tamano_bytes debe ser mayor que cero")
	}

	adjuntoID := uuid.New()

	// Metadata en bitacora (op_bitacora). estado_escaneo nace 'pendiente' (contrato).
	tx, err := s.bitacora.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx,
		`INSERT INTO bitacora.adjunto
		   (id, viaje_id, nombre, tipo_medio, tamano_bytes, checksum_sha256, estado_escaneo)
		 VALUES ($1, $2, $3, $4, $5, $6, 'pendiente')`,
		adjuntoID, viajeID, in.Nombre, in.TipoMedio, in.TamanoBytes, in.ChecksumSha256,
	)
	if err != nil {
		return nil, fmt.Errorf("operacion: insertar adjunto: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("operacion: commit crear adjunto: %w", err)
	}

	// Rastro de auditoría del acceso (E1-T7), en la tx del rol op_auditoria. El
	// acto auditado es la intención de subida; no hay más mutación de dominio que
	// encadenar (la metadata ya se escribió con su propio rol).
	if err := s.auditar(ctx, viajeID, accionSubida, actorID); err != nil {
		return nil, err
	}

	// URL de subida de vida corta. Los bytes no pasan por el servicio.
	url, expira, err := s.almacen.URLSubida(ctx, viajeID.String(), adjuntoID.String(), in.TipoMedio)
	if err != nil {
		return nil, fmt.Errorf("operacion: firmar url de subida: %w", err)
	}

	return &operclient.IntencionSubidaAdjunto{
		AdjuntoId: adjuntoID,
		UrlSubida: url,
		ExpiraEn:  expira,
	}, nil
}

// Descargar devuelve una URL de descarga de vida corta para un adjunto existente
// y deja el rastro de auditoría. Un adjunto inexistente (o de otro viaje) da 404
// antes de firmar nada. actorID es el sujeto del token (quién descarga).
//
// V1 no bloquea por estado_escaneo (shortcut: sin escáner que mueva el estado,
// la guarda bloquearía todas las descargas; se añade cuando exista el escáner).
func (s *Servicio) Descargar(ctx context.Context, viajeID, adjuntoID operclient.Id, actorID string) (*operclient.DescargaAdjunto, error) {
	tx, err := s.bitacora.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var tipoMedio string
	err = tx.QueryRow(ctx,
		`SELECT tipo_medio FROM bitacora.adjunto WHERE id = $1 AND viaje_id = $2`,
		adjuntoID, viajeID,
	).Scan(&tipoMedio)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, platformerrors.NewNoEncontrado("operacion: adjunto %s no encontrado en el viaje %s", adjuntoID, viajeID)
	}
	if err != nil {
		return nil, fmt.Errorf("operacion: leer adjunto: %w", err)
	}
	_ = tx.Rollback(ctx) // solo fue una lectura; la auditoría va en su propio rol

	if err := s.auditar(ctx, viajeID, accionDescarga, actorID); err != nil {
		return nil, err
	}

	url, expira, err := s.almacen.URLDescarga(ctx, viajeID.String(), adjuntoID.String())
	if err != nil {
		return nil, fmt.Errorf("operacion: firmar url de descarga: %w", err)
	}
	return &operclient.DescargaAdjunto{UrlDescarga: url, ExpiraEn: expira}, nil
}

// auditar escribe exactamente un registro probatorio con el interceptor (P3), en
// una transacción del rol op_auditoria. El cambio de dominio ya ocurrió (metadata
// insertada, o lectura para descarga); aquí solo se encadena el rastro, igual que
// en descargas.Registrar.
func (s *Servicio) auditar(ctx context.Context, viajeID operclient.Id, accion, actorID string) error {
	tx, err := s.auditoria.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	reg := auditoria.Registro{
		ViajeID: viajeID.String(),
		Modulo:  "auditoria",
		Accion:  accion,
		ActorID: actorID,
	}
	if err := auditoria.Interceptar(ctx, tx, reg, func(context.Context, pgx.Tx) error { return nil }); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("operacion: commit auditar %s: %w", accion, err)
	}
	return nil
}

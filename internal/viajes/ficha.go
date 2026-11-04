package viajes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// La ficha técnica del viaje (E8-T12, DEC-104) es lo que la solicitud y la
// asignación no cubren: tracto del cliente, contenedores y referencias de GPS.
// Vive en columnas de viajes.viaje (migración 0005). Todo es opcional y se edita
// por partes; ningún campo gatea transiciones en V1 (DEC-104 #3), así que la
// ficha se edita en cualquier estado del viaje.

// ObtenerFicha lee la ficha del viaje. 404 si el viaje no existe.
func (s *Servicio) ObtenerFicha(ctx context.Context, id operclient.Id) (*operclient.FichaTecnica, error) {
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return fichaDe(ctx, tx, id)
}

// GuardarFicha aplica un guardado parcial (guardarFicha, PATCH) y devuelve la
// ficha resultante:
//   - Tracto nil no toca el tracto. Un Tracto sin ningún dato lo borra (el
//     `"tracto": null` del cuerpo HTTP llega aquí como &Tracto{}).
//   - Contenedores/GpsIds nil no tocan la lista; presentes la REEMPLAZAN, y un
//     arreglo vacío la borra. Un contenedor sin datos se descarta; los gps_ids
//     repetidos se deduplican conservando el orden.
//   - Un texto vacío o de solo espacios cuenta como dato ausente.
//
// No valida los gps_ids contra catalogos: son referencias (DEC-104 #5), y el
// selector del panel ya ofrece solo dispositivos activos. 404 si el viaje no existe.
func (s *Servicio) GuardarFicha(ctx context.Context, id operclient.Id, in operclient.FichaTecnicaEntrada) (*operclient.FichaTecnica, error) {
	args := []any{id}
	var sets []string
	set := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if in.Tracto != nil {
		set("tracto_placas", texto(in.Tracto.Placas))
		set("tracto_economico", texto(in.Tracto.Economico))
		set("tracto_marca", texto(in.Tracto.Marca))
		set("tracto_modelo", texto(in.Tracto.Modelo))
		set("tracto_color", texto(in.Tracto.Color))
	}
	if in.Contenedores != nil {
		cs := []operclient.Contenedor{}
		for _, c := range *in.Contenedores {
			c = operclient.Contenedor{Economico: texto(c.Economico), Placas: texto(c.Placas), Sello: texto(c.Sello), Color: texto(c.Color)}
			if c != (operclient.Contenedor{}) {
				cs = append(cs, c)
			}
		}
		raw, err := json.Marshal(cs)
		if err != nil {
			return nil, fmt.Errorf("operacion: serializar contenedores: %w", err)
		}
		set("contenedores", string(raw))
	}
	if in.GpsIds != nil {
		ids := []uuid.UUID{}
		vistos := map[uuid.UUID]bool{}
		for _, g := range *in.GpsIds {
			if !vistos[g] {
				vistos[g] = true
				ids = append(ids, g)
			}
		}
		set("gps_ids", ids)
	}

	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if len(sets) > 0 {
		tag, err := tx.Exec(ctx, "UPDATE viajes.viaje SET "+strings.Join(sets, ", ")+" WHERE id = $1", args...)
		if err != nil {
			return nil, fmt.Errorf("operacion: guardar ficha: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil, platformerrors.NewNoEncontrado("operacion: viaje %s no encontrado", id)
		}
	}
	f, err := fichaDe(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("operacion: commit guardar ficha: %w", err)
	}
	return f, nil
}

// fichaDe lee la ficha dentro de tx. Las colecciones nunca van nil (forma estable).
func fichaDe(ctx context.Context, tx pgx.Tx, id operclient.Id) (*operclient.FichaTecnica, error) {
	var (
		t    operclient.Tracto
		raw  []byte
		gps  []uuid.UUID
		cont []operclient.Contenedor
	)
	err := tx.QueryRow(ctx,
		`SELECT tracto_placas, tracto_economico, tracto_marca, tracto_modelo, tracto_color, contenedores, gps_ids
		   FROM viajes.viaje WHERE id = $1`, id,
	).Scan(&t.Placas, &t.Economico, &t.Marca, &t.Modelo, &t.Color, &raw, &gps)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, platformerrors.NewNoEncontrado("operacion: viaje %s no encontrado", id)
	}
	if err != nil {
		return nil, fmt.Errorf("operacion: leer ficha: %w", err)
	}
	if err := json.Unmarshal(raw, &cont); err != nil {
		return nil, fmt.Errorf("operacion: leer contenedores de la ficha: %w", err)
	}
	if cont == nil {
		cont = []operclient.Contenedor{}
	}
	if gps == nil {
		gps = []uuid.UUID{}
	}
	f := &operclient.FichaTecnica{ViajeId: id, Contenedores: &cont, GpsIds: &gps}
	if t != (operclient.Tracto{}) {
		f.Tracto = &t
	}
	return f, nil
}

// texto normaliza un campo libre: ausente, vacío o de solo espacios ⇒ nil.
func texto(p *string) *string {
	if p == nil || strings.TrimSpace(*p) == "" {
		return nil
	}
	return p
}

package bitacora

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// encodeCursor y decodeCursor codifican el cursor keyset (creado_en, correlativo)
// como un token opaco base64. El keyset usa el correlativo interno (BIGSERIAL),
// no el UUID público derivado: la paginación ordena por la clave real de la
// tabla. Su forma interna no es contrato (el cliente lo devuelve tal cual).
func encodeCursor(t time.Time, correlativo int64) string {
	raw := t.UTC().Format(time.RFC3339Nano) + "|" + strconv.FormatInt(correlativo, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(s string) (time.Time, int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, 0, err
	}
	partes := strings.SplitN(string(raw), "|", 2)
	if len(partes) != 2 {
		return time.Time{}, 0, fmt.Errorf("cursor mal formado")
	}
	t, err := time.Parse(time.RFC3339Nano, partes[0])
	if err != nil {
		return time.Time{}, 0, err
	}
	correlativo, err := strconv.ParseInt(partes[1], 10, 64)
	if err != nil {
		return time.Time{}, 0, err
	}
	return t, correlativo, nil
}

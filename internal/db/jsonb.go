package db

import (
	"encoding/json"
	"fmt"
)

// JSONB prepares a value for a JSONB column. A nil map returns nil so the
// column receives NULL rather than an empty object.
//
// EVERY JSONB parameter in this codebase must go through here AND carry an
// explicit ::jsonb cast in the SQL. The reason is the simple protocol that
// PgBouncer forces (see pool.go):
//
// Under the extended protocol, pgx asks the server to describe each parameter,
// learns the target column is jsonb, and picks an encoder. The simple protocol
// has no describe step — pgx sees a bare Go map with no target type and gives
// up before the query is ever sent:
//
//	unable to encode map[string]interface{}{...} into text format
//	for unknown type (OID 0): cannot find encode plan
//
// A []byte is worse than an error: it encodes as a bytea literal and Postgres
// rejects it with "invalid input syntax for type json", which reads like bad
// data rather than a driver configuration problem.
//
// Marshalling to a string here and casting in the SQL is unambiguous under both
// protocols, so this is not a PgBouncer-only code path and there is no second
// behaviour to keep in sync.
func JSONB(v any) (any, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		if t == nil {
			return nil, nil
		}
	case []byte:
		if len(t) == 0 {
			return nil, nil
		}
		// Already JSON from the wire; pass the text through unchanged.
		return string(t), nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal jsonb parameter: %w", err)
	}
	return string(b), nil
}

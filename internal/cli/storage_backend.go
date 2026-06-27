package cli

import (
	"database/sql"
	"fmt"

	"vornik.io/vornik/internal/storage"
)

func requirePostgresDB(backend *storage.Backend, feature string) (*sql.DB, error) {
	if backend == nil {
		return nil, fmt.Errorf("%s requires a postgres database backend", feature)
	}
	if backend.Driver != "postgres" {
		return nil, fmt.Errorf("%s requires database.driver=postgres (current: %s)", feature, backend.Driver)
	}
	if backend.PG == nil || backend.PG.DB == nil {
		return nil, fmt.Errorf("%s requires an initialized postgres database backend", feature)
	}
	return backend.PG.DB, nil
}

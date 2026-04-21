package resource

import "github.com/jackc/pgx/v5/pgxpool"

type Config struct {
	DB *pgxpool.Pool
}

func NewConfig(db *pgxpool.Pool) Config {
	return Config{DB: db}
}

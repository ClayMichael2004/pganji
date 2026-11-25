package db

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"

	"github.com/golang-migrate/migrate/v4"
	pgxDriver "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

func RunMigrationsWithPool(migrationFS fs.FS, pool *pgxpool.Pool) error {
	driver, err := iofs.New(migrationFS, ".")
	if err != nil {
		return fmt.Errorf("failed to init iofs migration driver: %w", err)
	}

	// Create standard database connection from the verified pgx connection pool
	dbConn := stdlib.OpenDBFromPool(pool)

	dbInstance, err := pgxDriver.WithInstance(dbConn, &pgxDriver.Config{})
	if err != nil {
		return fmt.Errorf("failed to create database migration instance: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", driver, "postgres", dbInstance)
	if err != nil {
		return fmt.Errorf("failed to create migrate runner: %w", err)
	}
	defer m.Close()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migration run failed: %w", err)
	}

	slog.Info("database schema migrations applied successfully")
	return nil
}
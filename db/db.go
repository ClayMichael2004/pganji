package db

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func ConnectDB(ctx context.Context, connString string) (*pgxpool.Pool, error){
	//parse the db connection/url string
	config, err:= pgxpool.ParseConfig(connString)
	if err!=nil{
		return nil, fmt.Errorf("unable to parse database config: %w", err)
	}

	//context manages timeouts, deadlines and cancellations across api boundaries
	config.MaxConns=25
	config.MinConns=5
	config.MaxConnIdleTime=15 * time.Minute
	config.MaxConnLifetime=1*time.Hour

	//establish/creates a  connection pool
	pool, err:= pgxpool.NewWithConfig(ctx, config)
	if err !=nil{
		return nil, fmt.Errorf("unable to create connection pool: %w", err)
	}

	//ping the db to make sure its actually working. 
	//pool.Ping checks is the db is alive and reachable
	//pool.Close permanently shuts doiwn the pool and terminates all connections
	if err:= pool.Ping(ctx); err!=nil{
		pool.Close()
		return nil, fmt.Errorf("database ping failed: %w", err)
	}

	log.Println("Successfully connected to PostgreSql ")
	return pool, nil
}
package main

import (
	"context"
	"log"
	"os"
	"time"
	"pganji/db"
)

func main(){
	connStr:= os.Getenv("DATABASE_URL")
	if connStr==""{
		//fallback default for local dev
		connStr="postgres://localhost:5432/pganji?sslmode=disable"
	}

	//create a context with a 5 second timeout for the initial connection
	ctx, cancel:= context.WithTimeout(context.Background(), 5*time.Second)
	// Without defer cancel(): If your database connection succeeds in 5 milliseconds, the background timer keeps running in Go's memory for the full duration (e.g., 3 seconds). Under heavy traffic, thousands of these zombie timers will pile up, causing a major memory leak.With defer cancel(): The moment your function finishes connecting and returns the pool, the context is immediately destroyed, instantly freeing up system memory.
	defer cancel()

	pool, err:= db.ConnectDB(ctx, connStr)
	if err!=nil{
		log.Fatalf("Fatal Error: Could not connect to db; %v\n", err)
	}
	defer pool.Close()
	log.Println("Pganji initialized successfully")
}
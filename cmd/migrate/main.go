package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"robloxkit/internal/mysqlstore"
)

func main() {
	command := flag.String("command", "", "migration command: up, status, or version")
	flag.Parse()
	if *command == "" && flag.NArg() > 0 {
		*command = flag.Arg(0)
	}
	if *command != "up" && *command != "status" && *command != "version" {
		log.Printf("usage: migrate -command up|status|version")
		os.Exit(2)
	}
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		log.Print("MYSQL_DSN is required")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := mysqlstore.Open(ctx, dsn, mysqlstore.PoolConfig{})
	if err != nil {
		log.Printf("database open failed: %v", err)
		os.Exit(1)
	}
	defer db.Close()
	version, err := mysqlstore.Migrate(ctx, db, *command)
	if err != nil {
		log.Printf("migration failed: %v", err)
		os.Exit(1)
	}
	fmt.Printf("migration %s completed at version %d\n", *command, version)
}

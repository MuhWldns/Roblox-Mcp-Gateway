package main

import (
	"log"
	"os"

	"robloxkit/internal/appconfig"
)

func main() {
	if _, err := appconfig.LoadBridge(os.Getenv); err != nil {
		log.Printf("startup failed: %v", err)
		os.Exit(1)
	}
}

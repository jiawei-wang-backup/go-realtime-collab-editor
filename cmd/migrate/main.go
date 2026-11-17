package main

import (
	"context"
	"log"

	mysqlstorage "github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/adapter/mysql"
	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/migration"
	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/platform/config"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	if cfg.StorageDriver != config.StorageMySQL {
		log.Fatal("migrations require STORAGE_DRIVER=mysql")
	}
	store, err := mysqlstorage.Open(context.Background(), cfg.MySQL)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	if err := migration.Run(context.Background(), store.DB(), "migrations"); err != nil {
		log.Fatal(err)
	}
	log.Print("migrations applied")
}

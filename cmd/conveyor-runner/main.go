// conveyor-runner is the standalone local execution-plane daemon. It claims
// durable River jobs from Postgres and is the only process that needs Docker,
// worktrees, secret backends, or harness credentials (spec §3.2, §21.1).
package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/routing"
	"github.com/kidus-tiliksew/conveyor/internal/runner/localdocker"
	postgresstore "github.com/kidus-tiliksew/conveyor/internal/store/postgres"
)

func main() {
	configPath := flag.String("config", "conveyor.yaml", "path to runner/workspace config")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	deployment, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	packBundle, err := pack.Load(deployment.PackDir)
	if err != nil {
		log.Fatalf("load Phase 3 pack: %v", err)
	}
	if deployment.Database.Backend != "postgres" {
		log.Fatal("standalone runner requires the postgres backend")
	}
	st, err := postgresstore.Open(ctx, deployment.Database.URL)
	if err != nil {
		log.Fatalf("open Postgres store: %v", err)
	}
	defer st.Close()
	seeded, err := st.BootstrapWorkspaceConfig(ctx, deployment)
	if err != nil {
		log.Fatalf("bootstrap runner capacity: %v", err)
	}
	if !seeded {
		log.Printf("workspace %q already exists; ignoring workspace sections from %s", deployment.Workspace, *configPath)
	}
	cfg, err := st.RuntimeConfig(ctx, deployment)
	if err != nil {
		log.Fatalf("load database workspace config: %v", err)
	}

	local := localdocker.New()
	local.SecretResolver = cfg.SecretResolver()
	local.SecretPolicies = cfg.SecretPolicies()
	dispatcher := dispatch.New(st, gitx.NewManager(cfg.CacheDir, cfg.JobsDir), local, cfg)
	dispatcher.Pack = packBundle
	dispatcher.UseDurableQueue()
	dispatcher.ConfigProvider = func(ctx context.Context) (*config.Config, error) {
		return st.RuntimeConfig(ctx, deployment)
	}
	dispatcher.RouterFactory = func(cfg config.Routing) routing.Selector {
		return routing.New(st, cfg)
	}
	client, err := dispatch.NewRiverClient(st.Pool(), dispatcher)
	if err != nil {
		log.Fatalf("create River worker: %v", err)
	}
	if err := client.Start(ctx); err != nil {
		log.Fatalf("start River worker: %v", err)
	}
	log.Printf("local runner listening on durable workspace queue %q", cfg.Workspace)
	<-ctx.Done()
	stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.Stop(stopCtx); err != nil {
		log.Printf("stop River worker: %v", err)
	}
}

package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/relocode/relo/internal/app"
	"github.com/relocode/relo/internal/ipc"
	"github.com/relocode/relo/internal/logging"
)

var version = "dev"

func main() {
	os.Exit(run())
}

func run() int {
	var workspace, db, oc, rt, level string
	var showVersion bool
	flag.BoolVar(&showVersion, "version", false, "print version")
	flag.StringVar(&workspace, "workspace", "", "workspace path")
	flag.StringVar(&db, "db", "", "sqlite path")
	flag.StringVar(&oc, "opencode", "", "opencode binary")
	flag.StringVar(&rt, "runtime", "opencode", "opencode or mock")
	flag.StringVar(&level, "log-level", "info", "debug|info|warn|error")
	flag.Parse()
	if showVersion {
		fmt.Println(version)
		return 0
	}
	if workspace == "" {
		var err error
		workspace, err = os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	log := logging.New(os.Stderr, logging.ParseLevel(level)).With(map[string]any{"component": "relo-core"})
	core, err := app.New(app.Config{Workspace: workspace, DBPath: db, OCPath: oc, Runtime: rt, LogLevel: level}, log)
	if err != nil {
		log.Error("startup failed", map[string]any{"error": err.Error()})
		return 1
	}
	srv := ipc.New(os.Stdin, os.Stdout, core.Handle)
	core.AttachIPC(srv)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(ch)
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve() }()
	log.Info("relo core listening on stdio", map[string]any{"workspace": workspace, "runtime": rt})
	select {
	case err := <-serveDone:
		if err != nil {
			log.Error("ipc", map[string]any{"error": err.Error()})
		}
	case <-ch:
		srv.Shutdown()
		if err := <-serveDone; err != nil {
			log.Error("ipc", map[string]any{"error": err.Error()})
		}
	}
	core.Close()
	return 0
}

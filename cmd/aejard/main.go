// Command aejard is aejar's control-plane daemon.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	aejarv1 "github.com/eyanshu1997/aejar/gen/aejar/v1"
	"github.com/eyanshu1997/aejar/internal/auth"
	"github.com/eyanshu1997/aejar/internal/canon"
	"github.com/eyanshu1997/aejar/internal/runner"
	"github.com/eyanshu1997/aejar/internal/server"
	"github.com/eyanshu1997/aejar/internal/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type config struct {
	listen, dataDir, sessionsDir  string
	ownerEmails, oidcClientID     string
	spawnerAddr, spawnerTokenFile string
	tick                          time.Duration
}

func main() {
	home, _ := os.UserHomeDir()
	var c config
	flag.StringVar(&c.listen, "listen", "127.0.0.1:9111", "address to serve gRPC and REST on (nginx, sessions and the CLI all use it)")
	flag.StringVar(&c.dataDir, "data", filepath.Join(home, "aejar-data"), "data directory")
	flag.StringVar(&c.sessionsDir, "sessions", "", "session directories shared with the spawner (default <data>/sessions)")
	flag.StringVar(&c.ownerEmails, "owner-emails", "", "comma-separated Google accounts allowed to sign in through nginx")
	flag.StringVar(&c.oidcClientID, "oidc-client-id", "", "Google OAuth client id that ID tokens must be issued for")
	flag.StringVar(&c.spawnerAddr, "spawner", "", "aejar-spawner address; empty logs instead of starting sessions")
	flag.StringVar(&c.spawnerTokenFile, "spawner-token-file", "", "shared token for the spawner")
	flag.DurationVar(&c.tick, "tick", 10*time.Second, "scheduler interval")
	flag.Parse()
	if c.sessionsDir == "" {
		c.sessionsDir = filepath.Join(c.dataDir, "sessions")
	}
	if err := run(c); err != nil {
		log.Fatal(err)
	}
}

func run(c config) error {
	if err := os.MkdirAll(c.dataDir, 0o700); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, filepath.Join(c.dataDir, "aejar.db"))
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer db.Close()

	ownerToken, err := ownerToken(filepath.Join(c.dataDir, "owner.token"))
	if err != nil {
		return err
	}
	a := &auth.Authenticator{DB: db, OwnerTokenHash: canon.HashBytes([]byte(ownerToken))}
	if c.oidcClientID != "" || c.ownerEmails != "" {
		if c.oidcClientID == "" || c.ownerEmails == "" {
			return errors.New("browser login needs both -oidc-client-id and -owner-emails")
		}
		a.OIDC = auth.GoogleVerifier(c.oidcClientID)
		a.OwnerEmails = strings.Split(c.ownerEmails, ",")
	}

	var r runner.Runner = runner.Log{Logf: log.Printf}
	if c.spawnerAddr != "" {
		token, err := os.ReadFile(c.spawnerTokenFile)
		if err != nil {
			return fmt.Errorf("spawner token: %w", err)
		}
		conn, err := grpc.NewClient(c.spawnerAddr,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithPerRPCCredentials(bearer(strings.TrimSpace(string(token)))))
		if err != nil {
			return err
		}
		defer conn.Close()
		r = &runner.Spawner{Client: aejarv1.NewSpawnerServiceClient(conn), SessionsDir: c.sessionsDir}
	}

	srv := server.New(db, a, r)
	srv.SessionsDir = c.sessionsDir
	handler, cleanup, err := srv.Handler(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	go srv.Run(ctx, c.tick)

	hs := &http.Server{Addr: c.listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		hs.Shutdown(shutdown)
	}()
	log.Printf("aejard listening on %s (data %s, spawner %q)", c.listen, c.dataDir, c.spawnerAddr)
	if err := hs.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// bearer attaches a static bearer token to every RPC. The spawner is only
// reachable on the internal network, so plaintext transport is acceptable.
type bearer string

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}

func (bearer) RequireTransportSecurity() bool { return false }

// ownerToken reads the owner token, creating it (mode 0600) on first run.
func ownerToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	tok := auth.NewToken()
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	log.Printf("created owner token at %s", path)
	return tok, nil
}

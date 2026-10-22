// Command aejar-spawner is the only aejar process with access to the Docker
// socket. It starts session containers from a fixed template for aejard.
package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	aejarv1 "github.com/eyanshu1997/aejar/gen/aejar/v1"
	"github.com/eyanshu1997/aejar/internal/spawner"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func main() {
	var (
		listen       = flag.String("listen", ":9200", "address for aejard to reach the spawner on")
		tokenFile    = flag.String("token-file", "", "shared token aejard presents")
		claudeToken  = flag.String("claude-token-file", "", "Claude subscription token (claude setup-token)")
		coherenceKey = flag.String("coherence-key-file", "", "coherence API key for sessions (optional)")
		image        = flag.String("image", "aejar-session:latest", "session image")
		localDir     = flag.String("sessions", "/sessions", "session directories, as this process sees them")
		hostDir      = flag.String("host-sessions", "", "the same directory as the Docker daemon sees it")
		network      = flag.String("network", "aejar_sessions", "network for session containers")
		aejardURL    = flag.String("aejard-url", "http://aejard:9111", "aejard URL from inside sessions")
		coherenceURL = flag.String("coherence-url", "http://coherence:8080", "coherence URL from inside sessions")
		memoryMB     = flag.Int("memory-mb", 900, "memory limit per session")
		cpus         = flag.String("cpus", "1.5", "CPU limit per session")
		runtime      = flag.String("runtime", "", "optional OCI runtime (runsc)")
		dockerBinary = flag.String("docker", "docker", "docker CLI")
	)
	flag.Parse()
	if *hostDir == "" || *tokenFile == "" || *claudeToken == "" {
		log.Fatal("-host-sessions, -token-file and -claude-token-file are required")
	}
	token, err := readSecret(*tokenFile)
	if err != nil {
		log.Fatal(err)
	}
	claude, err := readSecret(*claudeToken)
	if err != nil {
		log.Fatal(err)
	}
	var cohKey string
	if *coherenceKey != "" {
		if cohKey, err = readSecret(*coherenceKey); err != nil {
			log.Fatal(err)
		}
	}

	s := &spawner.Spawner{
		Cfg: spawner.Config{
			Image: *image, LocalSessionsDir: *localDir, HostSessionsDir: *hostDir, Network: *network,
			AejardURL: *aejardURL, CoherenceURL: *coherenceURL, MemoryMB: *memoryMB, CPUs: *cpus,
			PidsLimit: 512, Runtime: *runtime, ClaudeToken: claude, CoherenceKey: cohKey,
			DockerBinary: *dockerBinary, StopTimeoutSec: 30,
		},
		Exec:  spawner.RealExec(*dockerBinary),
		Chown: os.Lchown,
	}
	gs := grpc.NewServer(grpc.UnaryInterceptor(requireToken(token)))
	aejarv1.RegisterSpawnerServiceServer(gs, s)

	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { <-ctx.Done(); gs.GracefulStop() }()
	log.Printf("aejar-spawner listening on %s (image %s, network %s)", *listen, *image, *network)
	if err := gs.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		log.Fatal(err)
	}
}

func readSecret(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return s, nil
}

func requireToken(token string) grpc.UnaryServerInterceptor {
	want := []byte("Bearer " + token)
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		for _, v := range md.Get("authorization") {
			if subtle.ConstantTimeCompare([]byte(v), want) == 1 {
				return h(ctx, req)
			}
		}
		return nil, status.Error(codes.Unauthenticated, "bad spawner token")
	}
}

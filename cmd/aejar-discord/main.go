// Command aejar-discord runs aejar's Discord bot: one thread per task, the
// session's replies and approval cards in it, slash commands, and thread
// replies forwarded to the session. Only the owner's Discord user is obeyed.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	aejarv1 "github.com/eyanshu1997/aejar/gen/aejar/v1"
	"github.com/eyanshu1997/aejar/internal/discordbot"
	"github.com/eyanshu1997/aejar/internal/spawner"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type bearer string

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}
func (bearer) RequireTransportSecurity() bool { return false }

func main() {
	var (
		tokenFile      = flag.String("token-file", "", "Discord bot token")
		guild          = flag.String("guild", "", "Discord server id")
		channel        = flag.String("channel", "", "channel aejar posts task threads in")
		owner          = flag.String("owner", "", "owner's Discord user id; nobody else is obeyed")
		aejard         = flag.String("aejard", "aejard:9111", "aejard gRPC address")
		ownerTokenFile = flag.String("owner-token-file", "", "aejard owner token")
		state          = flag.String("state", "/state/discord.json", "bot state file")
		publicURL      = flag.String("public-url", "", "public aejar URL for links")
	)
	flag.Parse()
	if *guild == "" || *channel == "" || *owner == "" {
		log.Fatal("-guild, -channel and -owner are required")
	}
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			log.Fatalf("read %s: %v", p, err)
		}
		return spawner.CleanToken(string(b))
	}
	conn, err := grpc.NewClient(*aejard, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(bearer(read(*ownerTokenFile))))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	s, err := discordgo.New("Bot " + read(*tokenFile))
	if err != nil {
		log.Fatal(err)
	}
	d := &discordbot.Discord{S: s, GuildID: *guild, ChannelID: *channel}
	bot := &discordbot.Bot{
		API: discordbot.API{
			Tasks: aejarv1.NewTaskServiceClient(conn), Approvals: aejarv1.NewApprovalServiceClient(conn),
			Audit: aejarv1.NewAuditServiceClient(conn), Ops: aejarv1.NewOpsServiceClient(conn),
		},
		Chat: d, OwnerID: *owner, PublicURL: strings.TrimRight(*publicURL, "/"), StatePath: *state,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for {
		if err = bot.Load(ctx); err == nil {
			break
		}
		log.Printf("waiting for aejard: %v", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
	if err := d.Wire(bot); err != nil {
		log.Fatalf("discord: %v", err)
	}
	defer s.Close()
	log.Printf("aejar-discord connected as %s (guild %s, channel %s)", s.State.User.Username, *guild, *channel)
	bot.Run(ctx, 2*time.Second)
	if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
		log.Print(err)
	}
}

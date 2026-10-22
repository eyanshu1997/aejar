// Command aejar is the owner's CLI for aejard. It speaks gRPC.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	aejarv1 "github.com/eyanshu1997/aejar/gen/aejar/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
)

const usage = `usage: aejar <command> [args]

  status                          scheduler and limit state
  ls                              list tasks
  show <task>                     task summary: state, recent events, pending approvals
  add [-class p0|p1|p2|p3] [-profile plan] <title> [prompt]
  prio <task> <bump>              add <bump> to a task's score
  pause <task> | resume <task>
  approvals                       pending approvals
  approve [-y] <approval> [reason]
  reject <approval> <reason>
  cap <n>                         request a cap change (needs approval)
  audit [-n 20]                   latest audit events
  audit verify                    verify the audit hash chain

Environment: AEJAR_ADDR (default 127.0.0.1:9012), AEJAR_TOKEN or AEJAR_TOKEN_FILE
(default ~/aejar-data/owner.token).`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "aejar:", err)
		os.Exit(1)
	}
}

type client struct {
	tasks     aejarv1.TaskServiceClient
	approvals aejarv1.ApprovalServiceClient
	audit     aejarv1.AuditServiceClient
	ops       aejarv1.OpsServiceClient
}

func dial() (*client, context.Context, error) {
	addr := envOr("AEJAR_ADDR", "127.0.0.1:9012")
	token := os.Getenv("AEJAR_TOKEN")
	if token == "" {
		home, _ := os.UserHomeDir()
		b, err := os.ReadFile(envOr("AEJAR_TOKEN_FILE", filepath.Join(home, "aejar-data", "owner.token")))
		if err != nil {
			return nil, nil, fmt.Errorf("owner token: %w", err)
		}
		token = strings.TrimSpace(string(b))
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, err
	}
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token)
	return &client{
		tasks:     aejarv1.NewTaskServiceClient(conn),
		approvals: aejarv1.NewApprovalServiceClient(conn),
		audit:     aejarv1.NewAuditServiceClient(conn),
		ops:       aejarv1.NewOpsServiceClient(conn),
	}, ctx, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func run(cmd string, args []string) error {
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		fmt.Println(usage)
		return nil
	}
	c, ctx, err := dial()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	defer tw.Flush()

	switch cmd {
	case "status":
		st, err := c.ops.GetStatus(ctx, &aejarv1.GetStatusRequest{})
		if err != nil {
			return err
		}
		fmt.Fprintf(tw, "cap\t%d\nrunning\t%d\nqueued\t%d\nwaiting\t%d\npending approvals\t%d\n",
			st.Cap, st.Running, st.Queued, st.Waiting, st.PendingApprovals)
		if st.LimitBlockedUntil != nil {
			fmt.Fprintf(tw, "usage limit until\t%s\n", st.LimitBlockedUntil.AsTime().Local().Format("Jan 2 15:04"))
		}
	case "ls":
		resp, err := c.tasks.ListTasks(ctx, &aejarv1.ListTasksRequest{})
		if err != nil {
			return err
		}
		fmt.Fprintln(tw, "ID\tSTATE\tCLASS\tSCORE\tPROFILE\tTITLE")
		for _, t := range resp.Tasks {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n", t.Id, short(t.State.String()), short(t.Class.String()), t.Score, t.Profile, t.Title)
		}
	case "show":
		if len(args) != 1 {
			return errors.New("usage: aejar show <task>")
		}
		sum, err := c.tasks.SummarizeTask(ctx, &aejarv1.SummarizeTaskRequest{Id: args[0]})
		if err != nil {
			return err
		}
		t := sum.Task
		fmt.Fprintf(tw, "task\t%s\ntitle\t%s\nstate\t%s %s\nclass\t%s (score %d)\nprofile\t%s\n",
			t.Id, t.Title, short(t.State.String()), t.WaitReason, short(t.Class.String()), t.Score, t.Profile)
		for _, a := range sum.PendingApprovals {
			fmt.Fprintf(tw, "pending\t%s %s: %s\n", a.Id, short(a.Class.String()), a.Summary)
		}
		for _, ev := range sum.RecentEvents {
			fmt.Fprintf(tw, "event\t%s %s %s\n", ev.Ts.AsTime().Local().Format("15:04:05"), ev.Actor, ev.Action)
		}
	case "add":
		fs := flag.NewFlagSet("add", flag.ContinueOnError)
		class := fs.String("class", "p1", "p0..p3")
		profile := fs.String("profile", "plan", "session profile")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() < 1 {
			return errors.New("usage: aejar add <title> [prompt]")
		}
		cv, ok := aejarv1.TaskClass_value["TASK_CLASS_"+strings.ToUpper(*class)]
		if !ok {
			return fmt.Errorf("unknown class %q", *class)
		}
		t, err := c.tasks.CreateTask(ctx, &aejarv1.CreateTaskRequest{
			Title: fs.Arg(0), Prompt: strings.Join(fs.Args()[1:], " "), Class: aejarv1.TaskClass(cv), Profile: *profile,
		})
		if err != nil {
			return err
		}
		fmt.Println(t.Id)
	case "prio":
		if len(args) != 2 {
			return errors.New("usage: aejar prio <task> <bump>")
		}
		n, err := strconv.Atoi(args[1])
		if err != nil {
			return err
		}
		t, err := c.tasks.SetPriority(ctx, &aejarv1.SetPriorityRequest{Id: args[0], UserBump: int32(n)})
		if err != nil {
			return err
		}
		fmt.Printf("%s score %d\n", t.Id, t.Score)
	case "pause", "resume":
		if len(args) != 1 {
			return fmt.Errorf("usage: aejar %s <task>", cmd)
		}
		var t *aejarv1.Task
		if cmd == "pause" {
			t, err = c.tasks.PauseTask(ctx, &aejarv1.PauseTaskRequest{Id: args[0]})
		} else {
			t, err = c.tasks.ResumeTask(ctx, &aejarv1.ResumeTaskRequest{Id: args[0]})
		}
		if err != nil {
			return err
		}
		fmt.Printf("%s %s\n", t.Id, short(t.State.String()))
	case "approvals":
		resp, err := c.approvals.ListApprovals(ctx, &aejarv1.ListApprovalsRequest{
			States: []aejarv1.ApprovalState{aejarv1.ApprovalState_APPROVAL_STATE_PENDING},
		})
		if err != nil {
			return err
		}
		fmt.Fprintln(tw, "ID\tCLASS\tTASK\tTOOL\tSUMMARY")
		for _, a := range resp.Approvals {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", a.Id, short(a.Class.String()), a.TaskId, a.ToolName, a.Summary)
		}
	case "approve", "reject":
		return decide(ctx, c, cmd == "approve", args)
	case "cap":
		if len(args) != 1 {
			return errors.New("usage: aejar cap <n>")
		}
		n, err := strconv.Atoi(args[0])
		if err != nil {
			return err
		}
		a, err := c.ops.RequestCapChange(ctx, &aejarv1.RequestCapChangeRequest{Cap: int32(n)})
		if err != nil {
			return err
		}
		fmt.Printf("created %s; run: aejar approve %s\n", a.Id, a.Id)
	case "audit":
		if len(args) == 1 && args[0] == "verify" {
			r, err := c.audit.VerifyAudit(ctx, &aejarv1.VerifyAuditRequest{})
			if err != nil {
				return err
			}
			if !r.Ok {
				return fmt.Errorf("audit chain BROKEN at seq %d (%d events)", r.BrokenAtSeq, r.Events)
			}
			fmt.Printf("ok: %d events, head %s\n", r.Events, r.HeadHash)
			return nil
		}
		fs := flag.NewFlagSet("audit", flag.ContinueOnError)
		n := fs.Int("n", 20, "events")
		if err := fs.Parse(args); err != nil {
			return err
		}
		resp, err := c.audit.ListAuditEvents(ctx, &aejarv1.ListAuditEventsRequest{Limit: 1000})
		if err != nil {
			return err
		}
		events := resp.Events
		if len(events) > *n {
			events = events[len(events)-*n:]
		}
		for _, ev := range events {
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", ev.Seq, ev.Ts.AsTime().Local().Format("Jan 2 15:04:05"), ev.Actor, ev.Action, ev.Subject)
		}
	default:
		return fmt.Errorf("unknown command %q\n%s", cmd, usage)
	}
	return nil
}

// decide shows the exact payload before approving: the decision carries its
// hash, so what is approved is what was shown.
func decide(ctx context.Context, c *client, approve bool, args []string) error {
	fs := flag.NewFlagSet("decide", flag.ContinueOnError)
	yes := fs.Bool("y", false, "do not ask for confirmation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 || (!approve && fs.NArg() < 2) {
		return errors.New("usage: aejar approve [-y] <approval> [reason] | aejar reject <approval> <reason>")
	}
	a, err := c.approvals.GetApproval(ctx, &aejarv1.GetApprovalRequest{Id: fs.Arg(0)})
	if err != nil {
		return err
	}
	payload, _ := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(a.Payload)
	fmt.Printf("%s  %s  task %s\n%s\n", a.Id, short(a.Class.String()), a.TaskId, a.Summary)
	if a.ToolName != "" {
		fmt.Printf("tool: %s\n", a.ToolName)
	}
	fmt.Printf("payload:\n%s\n", payload)
	if approve && !*yes {
		fmt.Print("approve exactly this? [y/N] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if strings.TrimSpace(strings.ToLower(line)) != "y" {
			return errors.New("not approved")
		}
	}
	d, err := c.approvals.DecideApproval(ctx, &aejarv1.DecideApprovalRequest{
		Id: a.Id, Approve: approve, Reason: strings.Join(fs.Args()[1:], " "), PayloadHash: a.PayloadHash,
	})
	if err != nil {
		return err
	}
	fmt.Printf("%s %s\n", d.Id, short(d.State.String()))
	return nil
}

// short trims enum prefixes: TASK_STATE_QUEUED -> queued.
func short(s string) string {
	for _, p := range []string{"TASK_STATE_", "TASK_CLASS_", "APPROVAL_CLASS_", "APPROVAL_STATE_"} {
		s = strings.TrimPrefix(s, p)
	}
	return strings.ToLower(s)
}

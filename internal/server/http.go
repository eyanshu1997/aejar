package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"

	aejarv1 "github.com/eyanshu1997/aejar/gen/aejar/v1"
	"github.com/eyanshu1997/aejar/internal/auth"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/encoding/protojson"
)

// Handler serves gRPC (for the CLI) and the grpc-gateway REST/JSON API (for
// the web UI, the Discord bot and Claude Code hooks) on one port. Both go
// through the same gRPC server, so the auth interceptor covers every route.
// The returned func releases the internal gateway connection.
func (s *Server) Handler(ctx context.Context) (http.Handler, func(), error) {
	gs := grpc.NewServer(grpc.UnaryInterceptor(s.Auth.Unary))
	aejarv1.RegisterTaskServiceServer(gs, s)
	aejarv1.RegisterApprovalServiceServer(gs, s)
	aejarv1.RegisterAuditServiceServer(gs, s)
	aejarv1.RegisterOpsServiceServer(gs, s)
	aejarv1.RegisterHookServiceServer(gs, s)

	// The gateway reaches the gRPC server over an in-memory listener instead
	// of calling the services directly, so interceptors apply to REST too.
	lis := bufconn.Listen(1 << 20)
	go gs.Serve(lis)
	conn, err := grpc.NewClient("passthrough:///aejard",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, err
	}

	gw := runtime.NewServeMux(
		runtime.WithMarshalerOption(runtime.MIMEWildcard, &runtime.JSONPb{
			MarshalOptions: protojson.MarshalOptions{EmitUnpopulated: false},
			// Claude Code hook bodies carry fields aejar does not model.
			UnmarshalOptions: protojson.UnmarshalOptions{DiscardUnknown: true},
		}),
		runtime.WithIncomingHeaderMatcher(func(key string) (string, bool) {
			if strings.EqualFold(key, auth.IDTokenHeader) {
				return auth.IDTokenHeader, true
			}
			return runtime.DefaultHeaderMatcher(key)
		}),
	)
	for _, register := range []func(context.Context, *runtime.ServeMux, *grpc.ClientConn) error{
		aejarv1.RegisterTaskServiceHandler,
		aejarv1.RegisterApprovalServiceHandler,
		aejarv1.RegisterAuditServiceHandler,
		aejarv1.RegisterOpsServiceHandler,
		aejarv1.RegisterHookServiceHandler,
	} {
		if err := register(ctx, gw, conn); err != nil {
			conn.Close()
			return nil, nil, err
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.DB.PingContext(r.Context()); err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/metrics", s.metrics)
	mux.Handle("/", gw)

	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			gs.ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
	cleanup := func() {
		conn.Close()
		gs.Stop()
	}
	return h2c.NewHandler(root, &http2.Server{}), cleanup, nil
}

// metrics serves Prometheus text format. Unauthenticated by design: it holds
// counts only, and aejard binds to localhost.
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	rows, err := s.DB.QueryContext(ctx, `SELECT state, COUNT(*) FROM tasks GROUP BY state`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	fmt.Fprintln(w, "# TYPE aejar_tasks gauge")
	for rows.Next() {
		var state, n int
		rows.Scan(&state, &n)
		fmt.Fprintf(w, "aejar_tasks{state=%q} %d\n", aejarv1.TaskState(state).String(), n)
	}
	rows.Close()
	st, err := s.GetStatus(ctx, &aejarv1.GetStatusRequest{})
	if err == nil {
		fmt.Fprintf(w, "# TYPE aejar_cap gauge\naejar_cap %d\n", st.Cap)
		fmt.Fprintf(w, "# TYPE aejar_approvals_pending gauge\naejar_approvals_pending %d\n", st.PendingApprovals)
		blocked := 0
		if st.LimitBlockedUntil != nil {
			blocked = 1
		}
		fmt.Fprintf(w, "# TYPE aejar_limit_blocked gauge\naejar_limit_blocked %d\n", blocked)
	}
	var denied int
	s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE action IN ('tool.denied', 'tool.needs_approval')`).Scan(&denied)
	fmt.Fprintf(w, "# TYPE aejar_tool_denials_total counter\naejar_tool_denials_total %d\n", denied)
}

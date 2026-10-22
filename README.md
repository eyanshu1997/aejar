# aejar

Personal AI control plane: runs isolated Claude Code sessions for tasks, gates
every external action behind the owner's approval, and audits everything.
Design docs: `docs/PLAN.md`, `docs/IMPLEMENTATION.md`, `docs/INTEGRATIONS.md`.

## Layout

- `proto/aejar/v1/` is the API source of truth. Every service is gRPC with
  `google.api.http` annotations; grpc-gateway serves the same API as REST/JSON
  (web UI, Discord bot, Claude Code hooks) and `gen/openapi/` holds the OpenAPI spec.
- `cmd/aejard` is the daemon (gRPC + REST on one port, default `127.0.0.1:9011`).
- `cmd/aejar` is the owner CLI (gRPC).
- `internal/policy` holds the tool risk tiers and the PreToolUse gate.
- `internal/scheduler` holds priority, caps, guards and preemption (pure functions).
- `internal/audit` is the hash-chained audit log.
- `coherence/` is the coherence submodule (docs and comments UI).

## Develop

    make gen     # after editing proto/
    make test    # vet + unit + e2e
    make build   # bin/aejard, bin/aejar

    bin/aejard -data ./data            # creates ./data/owner.token on first run
    AEJAR_TOKEN_FILE=./data/owner.token bin/aejar status

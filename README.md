# Stripe-lite Payment Gateway (Sprint 1 Foundation)

Contract-first Go HTTP payment gateway Stripe-lite. Designed for ultra-low latency, zero unexpected heap allocations in routing paths, and absolute single-source-of-truth (SSOT) contract compliance across engineering squads.

---

## 🏛 Architecture & Engineering Standards

- **Contract-First Single Source of Truth**: All API routes, payloads, parameters, and security schemes are declaratively defined in [`api/openapi.yaml`](api/openapi.yaml) under [OpenAPI 3.0.3](https://spec.openapis.org/oas/v3.0.3.html).
- **Strict Server Decoupling**: Handlers are code-generated via `oapi-codegen` using the `strict-server` paradigm (`internal/api/v1/v1.gen.go`). Domain handlers receive strongly-typed request structs and return strongly-typed response interfaces, eliminating raw `gin.Context` serialization errors.
- **RFC 7807 Problem Details**: HTTP error envelopes strictly adhere to [RFC 7807 (Problem Details for HTTP APIs)](https://datatracker.ietf.org/doc/html/rfc7807) with MIME type `application/problem+json`.
- **Zero-Dependency Localized Tooling**: Tools (`oapi-codegen`) are pinned in [`tools/tools.go`](tools/tools.go) and compiled directly into `./bin/`. No packages are ever installed into global system directories (`$GOPATH/bin` or `/usr/local/bin`).
- **Single-Binary Cohesion**: Swagger UI interactive documentation, JSON schemas, and YAML specifications are embedded directly into the Go binary at compile-time via `go:embed`.

---

## 📁 Monorepo Layout

```
stripe-lite/
├── api/
│   ├── openapi.yaml              # Canonical OpenAPI 3.0.3 SSOT specification
│   └── openapi_test.go           # CI contract & schema validation test
├── cmd/
│   ├── lint-spec/
│   │   └── main.go               # In-tree zero-dependency OpenAPI contract linter
│   └── server/
│       └── main.go               # Production Gin gateway with graceful teardown
├── configs/
│   └── config.yaml               # Declarative default runtime configuration
├── internal/
│   ├── api/v1/
│   │   ├── auth.go               # Identity domain handler receiver (Owner: Khánh)
│   │   ├── health.go             # Observability & liveness probe receiver
│   │   ├── payments.go           # Core engine charge receiver (Owner: Long)
│   │   ├── webhooks.go           # Webhook dispatcher receiver (Owner: Sơn)
│   │   ├── server.go             # Server struct & RFC 7807 problem constructors
│   │   ├── spec.go               # Spec extraction helpers
│   │   └── v1.gen.go             # Generated strict interfaces, models, and Gin router
│   ├── config/
│   │   └── config.go             # Typed runtime configuration loader
│   └── docs/
│       └── docs.go               # Embedded Swagger UI & raw spec HTTP handlers
├── scripts/
│   └── verify.sh                 # End-to-end HTTP integration test harness
├── tools/
│   └── tools.go                  # Developer tool dependencies (oapi-codegen)
├── Makefile                      # Tooling orchestration (codegen, build, test, verify)
├── oapi-codegen.yaml             # Code generation pipeline configuration
└── go.mod                        # Go module definition (Go 1.25)
```

---

## 🛠 Prerequisites

- **Go**: Version `1.25` or newer
- **Make**: Standard GNU `make`
- **Bash & curl**: For gateway smoke verification; no DB or Docker is required.
- **jq & OpenSSL**: Only for full Payments verification with a test database.
- **PostgreSQL 16 or Docker**: For `make test-integration`. Full Payments verification uses `PAYMENTS_TEST_DATABASE_URL`; database provisioning is separate.
- **Node.js & npx**: Required to bundle and lint the OpenAPI specification

---

## 🚀 Quick Start (How to Launch)

### 1. Build and Run Directly

To generate code, compile the release binary into `./bin/server`, and start the gateway on port `8080`:

```bash
make run
```

### 2. Custom Port & Environment Configuration

The gateway reads from environment variables with production fallbacks:

```bash
SERVER_PORT=9000 SERVER_HOST=127.0.0.1 APP_ENV=production make run
```

| Environment Variable | Default Value | Description |
| :--- | :--- | :--- |
| `SERVER_PORT` | `8080` | Port for the HTTP gateway |
| `SERVER_HOST` | `0.0.0.0` | Network interface to bind |
| `SERVER_BASE_PATH` | `/v1` | Base API routing namespace |
| `APP_ENV` | `development` | `development` (Gin debug mode) or `production` (Gin release mode) |

---

## 🔍 Interactive Endpoints

Once the server is running, access:

| Endpoint | Method | Format | Description |
| :--- | :--- | :--- | :--- |
| `http://localhost:8080/docs` | `GET` | HTML | Interactive Swagger UI (Embedded SPA) |
| `http://localhost:8080/openapi.json` | `GET` | JSON | Machine-readable OpenAPI specification (JSON) |
| `http://localhost:8080/openapi.yaml` | `GET` | YAML | Machine-readable OpenAPI specification (YAML) |
| `http://localhost:8080/v1/healthz` | `GET` | JSON | Liveness & operational readiness probe |
| `http://localhost:8080/healthz` | `GET` | Redirect | Root alias redirecting orchestrators to `/v1/healthz` |

---

## 📖 Makefile Reference

| Target | Description |
| :--- | :--- |
| `make all` | Executes complete pipeline: `lint-spec`, `codegen`, `test`, `build`. |
| `make codegen` | Builds local `./bin/oapi-codegen` and regenerates `internal/api/v1/v1.gen.go`. |
| `make lint-spec` | Fast sub-millisecond in-tree contract validation using `kin-openapi`. |
| `make lint-redocly` | Runs strict Redocly style & security audit via localized `npx`. |
| `make build` | Compiles optimized, stripped server binary to `./bin/server`. |
| `make run` | Builds binary and boots the HTTP gateway. |
| `make test` | Runs unit and contract tests with the race detector; no database or Docker is required. |
| `make test-integration` | Runs PostgreSQL integration tests and unit tests. Uses disposable Docker PostgreSQL unless test database URLs are supplied. |
| `make verify` | Runs gateway HTTP smoke tests without DB/Docker. Also runs Payments assertions when `PAYMENTS_TEST_DATABASE_URL` is set. |
| `make clean` | Removes `./bin/` artifacts and generated code. |

---

## 👥 Team Onboarding & Merge-Conflict Prevention Protocol

To work concurrently on separate Git branches without merge conflicts:

1. **Do Not Touch Generated Code**: Never manually modify `internal/api/v1/v1.gen.go`.
2. **Develop Exclusively Within Domain Boundaries**:
   - **Khánh (Identity)**: Implements authentication services under `internal/auth/` and wires into `internal/api/v1/auth.go`.
   - **Long (Payments)**: Implements the core payment engine under `internal/payments/` and wires into `internal/api/v1/payments.go`.
   - **Sơn (Webhooks)**: Implements the outbound event dispatcher under `internal/webhooks/` and wires into `internal/api/v1/webhooks.go`.
3. **Spec Evolution Rule**:
   - Schema modifications to `api/openapi.yaml` must be proposed and agreed upon prior to code generation.
   - Always run `make lint-spec && make lint-redocly && make codegen` before opening a pull request.

## Payments

See [the Payments guide](api/payments/README.md) for state rules, ER diagrams, migrations, credentials, and development configuration.
The root `api/openapi.yaml` declares the Payments paths and the `/v1` prefix directly.
The real server uses `/v1`. Auth and Webhooks remain assigned to their domain owners.
Payment processing uses an explicit development adapter in the verification suite.
No real processor adapter is included in Sprint 1.

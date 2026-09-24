module github.com/tbilol/dxrl/services/auth-gateway

go 1.26.0

toolchain go1.27.1

require (
	buf.build/go/protovalidate v1.4.0
	github.com/alicebob/miniredis/v2 v2.39.0
	github.com/caarlos0/env/v11 v11.4.1
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/google/uuid v1.6.0
	github.com/jackc/pgx/v5 v5.11.0
	github.com/pashagolub/pgxmock/v5 v5.2.0
	github.com/pressly/goose/v3 v3.28.0
	github.com/redis/go-redis/v9 v9.22.0
	github.com/tbilol/dxrl/gen/go v0.0.0-00010101000000-000000000000
	github.com/twmb/franz-go v1.22.0
	github.com/twmb/franz-go/pkg/kfake v0.0.0-20260918165059-79db0ce448c6
	golang.org/x/crypto v0.55.0
	google.golang.org/grpc v1.84.0
	google.golang.org/protobuf v1.36.12
)

require (
	buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go v1.36.12-20260825204119-511051f7f437.1 // indirect
	cel.dev/cel-go v0.32.0 // indirect
	cel.dev/expr v0.25.3 // indirect
	github.com/antlr4-go/antlr/v4 v4.13.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/klauspost/compress v1.20.0 // indirect
	github.com/mfridman/interpolate v0.0.2 // indirect
	github.com/pierrec/lz4/v4 v4.1.30 // indirect
	github.com/sethvargo/go-retry v0.4.0 // indirect
	github.com/twmb/franz-go/pkg/kmsg v1.14.0 // indirect
	github.com/yuin/gopher-lua v1.1.1 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/exp v0.0.0-20260824195058-e88cd73687aa // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260819154853-08b0e4226688 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260831171406-18b4a7587f8a // indirect
)

// Generated code from `make gen` (back/gen/go, git-ignored). The go.mod there
// is written by `make gen` from gen-go.mod.tmpl.
replace github.com/tbilol/dxrl/gen/go => ../../gen/go

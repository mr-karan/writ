# Benchmark results

This file holds measurements of the control plane.

## A dev control plane, 2026-11-10

The design of `writ serve --dev` set two targets: under 5 ms for a test's in-process control plane, and under 10 ms for `writ serve --dev` to answer `GET /v1/server`. Both runs used the AMD Ryzen AI 7 PRO 350 with 16 threads, with `/tmp` on tmpfs.

**In process.** `BenchmarkStart` in `internal/servetest` opens a new SQLite control database in a temporary directory, applies every migration, starts the server on `httptest`, and gets `GET /v1/server`. `go test -run '^$' -bench BenchmarkStart -benchtime 200x -count 5 ./internal/servetest/`:

```
BenchmarkStart-16    	     200	   2323817 ns/op
BenchmarkStart-16    	     200	   2103155 ns/op
BenchmarkStart-16    	     200	   2055221 ns/op
BenchmarkStart-16    	     200	   1966256 ns/op
BenchmarkStart-16    	     200	   1941199 ns/op
```

About 2 ms each.

**The binary.** A script that isn't in the repository started `writ serve --dev --data DIR --listen 127.0.0.1:7351` on a new data directory 30 times and polled `GET /v1/server` every 0.5 ms until it answered with `dev: true`, so the time includes starting the process and up to 0.5 ms of polling. Its output:

```
runs 30  median 8.5 ms  min 7.2 ms  max 12.5 ms
```

The first start in a new home directory printed `ready in 42 ms`, measured inside the process. These numbers do not include a plan: a plan through the dev control plane adds a request, a poll, and the plan itself.

# Staged console password cost

ADR 014 step 1 adds password contracts without enabling account authentication.
The implementation pins `golang.org/x/crypto` to v0.58.0 and uses its
[scrypt API](https://pkg.go.dev/golang.org/x/crypto/scrypt#Key) and
[Argon2id API](https://pkg.go.dev/golang.org/x/crypto/argon2#IDKey).
`internal/consoleauth` has no dependency on the store. Run creation and verification
before entering a store transaction; later authentication must recheck the account
version in the transaction that accepts the result.

The accepted profiles remain exactly the ADR profiles:

| Use | Cost | Salt / key | Principal allocation |
|---|---|---|---|
| Imported scrypt verification | n=16384, r=8, p=1 | 16 / 32 bytes | 16 MiB |
| New passwords and Argon2id verification | version 19, m=65536 KiB, t=3, p=1 | 16 / 32 bytes | 64 MiB |

Both operations share two process-wide admission slots, with immediate refusal
when full and no queue. At most 128 MiB of principal KDF allocations can be active
at once. This is not an RSS limit: Go runtime overhead and allocations retained
until garbage collection also consume memory. The exact accepted scrypt profile
also fixes its secondary buffers to 3072 bytes; measured total allocations include
those buffers, parsing and library/runtime overhead. Unsupported profiles are
rejected before KDF admission, including values whose cost products would overflow.
Records are limited to 256 bytes and submitted passwords to 1024 UTF-8 bytes.

Measured 2026-11-08 on Linux amd64, Intel N97 (four CPUs), Go 1.27.3,
`CGO_ENABLED=0`, using synthetic inputs only:

```text
go test ./internal/consoleauth -run '^$' -bench . -benchmem -count=3 -benchtime=5x
```

| Benchmark | Time per operation, three samples | Allocated bytes per operation |
|---|---|---|
| Scrypt verification | 96.8 / 86.9 / 94.0 ms | 16,782,174–16,783,286 |
| Argon2id creation | 302.2 / 265.8 / 269.4 ms | 67,111,656–67,111,838 |

The benchmark calls the credential package directly and never opens or locks a
store. Creation includes random salt generation and encoding. These small samples
show cost on this host, not latency guarantees or results for other architectures.
Repeat on each supported deployment platform before enabling account mode. The
admission bound supplements the login attempt quotas and transactional revocation
checks added in ADR 014 step 3. Account routes remain gated pending the later UI
and cutover steps.

Sonar rule `go:S5344` recommends stronger scrypt parameters. ADR 014 accepts this
profile only to verify existing hashes: raising the cost changes the derived key
and breaks compatibility. The single legacy verification call therefore carries
a documented `NOSONAR` exception; creation always uses Argon2id. Sonar's
[line suppression](https://docs.sonarsource.com/sonarqube-cloud/managing-your-projects/issues/editing)
affects every finding on that one line, so changes to that call require review.
No file, project or coverage exclusion is added.

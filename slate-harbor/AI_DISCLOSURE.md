# AI_DISCLOSURE.md


## Tools used

- **Claude ** — used as a pair-programmer and reviewer throughout
  implementation and documentation.

## Where AI was used, and how much

**Substantial.** AI was used heavily for code generation, test scaffolding, and
drafting documentation. Specifically:

- **Endpoint implementations** (`GET /wallets`, `credit`, `purchase`, `claim`):
  the handler and database-layer code was largely AI-generated from an
  agreed-upon design, then reviewed, run, and verified by me against the live
  service. I confirmed each endpoint's behaviour with manual requests before
  accepting it.
- **The exactly-once / idempotency mechanism** (insert-key-in-same-transaction,
  replay-on-conflict, request-hash mismatch handling): designed and written with
  AI assistance.
- **Tests:** the concurrency/duplicate integration tests (`test/`) and the
  `kill -9` crash-durability script were AI-scaffolded; I ran them, confirmed they
  pass, and used them to validate the durability and exactly-once claims.
- **Documentation:** `DESIGN.md` and `README.md` were drafted with AI from the
  actual code; I reviewed every claim against the implementation.

## What I did myself

- All environment setup, debugging, and verification: Docker build issues, the
  Go toolchain/version mismatch, the `.gitignore` rule that was silently excluding
  `cmd/server/`, and the Windows/PowerShell request tooling.
- Running every test and manual check, and reading the results.
- The design decisions I was asked to make and justify (datastore choice,
  isolation strategy, idempotency-key retention window, zero-state vs 404 on
  unknown players, the success/error status codes).
- Reviewing all generated code before committing it, and the git history.

## Honesty note

I have not represented any AI-generated behaviour as implemented when it is not, and
I have not claimed in the docs any property the code does not have. I can explain the
reasoning behind the idempotency ordering, the row-level locking that prevents
double-spend, and the WAL/commit durability behaviour in my own words.


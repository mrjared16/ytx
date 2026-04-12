# PO Token (Botguard) Engine

This document details the low-level technical architecture of the `--po` (Proof of Origin) Botguard minting pipeline designed to bypass YouTube's heavy IP throttling walls while maximizing runtime response times.

## Overview

When YouTube aggressively flags an IP address or requires signed attestation (especially on Music Premium tracks), standard streams will yield `HTTP 403 Forbidden`. The `--po` flag opts the extraction pipeline into a dedicated subprocess lane that mounts a simulated DOM environment, executes an obfuscated JS challenge sent by Google's attestation endpoints, and mints an encrypted token (`po_token`).

Because evaluating this Botguard payload natively blocks the UI thread for ~4.5 seconds, the `ytx` PO Mode uses aggressive off-critical-path caching.

## The PO State Machine

### 1. Attestation Block Fetch
A background request is fired immediately at the `watch?v=` HTML payload to extract a unique 195+ character `Botguard Challenge` encoded in the Google configuration objects. 

### 2. Node/Bun JSDOM Payload
Instead of heavily polyfilling the native `quickjs` (which is highly restricted and synchronous), PO mode routes the Botguard execution to an external `bun` (or `node`) persistent subprocess. 

The subprocess uses `JSDOM` to establish an artificial `window.document` capable of intercepting and executing the obfuscated script without crashing.

### 3. Asynchronous Coordination
Because fetching the challenge, booting the subprocess, and minting the Token is heavily CPU-bound (taking ~4100ms in modern Bun), this work is fully decoupled from the core `Cipher` initialization.
- The `fetchPlayerJSContext` and API endpoints continue their fetching while `poc_mint` operates concurrently.

## Optimization Strategy (Fastest Response Time)
Our explicit goal is to drop the 4.6s PO penalty down to matching the unauthenticated 300ms stream speeds.

### Dedicated Persistent Caching
To minimize process overhead across multiple CLI invocations, `ytx` persists the Botguard token directly to a `po_token.json` disk cache along with the current `visitorData` fingerprint. 
By saving this cache cross-session, any subsequent run skips the 4.1s execution payload entirely and immediately pipes the JWT into the final stream extraction API parameters.

**Benchmark Impact:**
- **PO Cold Start:** `~4600ms` (Subprocess mint heavily bound to CPU).
- **PO Warm Start:** `~330ms` (Botguard engine completely skipped. 100% harmonized with standard pipeline execution speeds).

### Persistent Subprocess (Bulk Mode)
When running across multiple tracks via the `--bulk` pipeline, PO mode uses a pre-booted persistent `bun` subprocess stream. Instead of incurring the 150ms `node` cold start overhead on every individual track, the engine streams JSON challenges to the live daemon, yielding sub-10ms inter-track PO challenge responses.

## Invariants
- **Global Key:** The cache strictly verifies that the `visitorData` from the initial HTML session matches the local cache block payload.
- **Fail-Fast:** If Botguard minting outright fails, `ytx` returns an explicit `POFailureTokenUnavailable` error rather than blindly passing an undecryptable payload and forcing a 403.

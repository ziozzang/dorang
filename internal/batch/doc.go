// Package batch implements the OpenAI-shaped Batch and Files surface on
// dorang's own scheduler.
//
// # Why the scheduler is dorang's own
//
// Neither self-hosted engine dorang targets has a batch job API. vLLM registers
// no /v1/batches route at all — its /v1/chat/completions/batch is a synchronous,
// single-request, all-or-nothing fan-out, and run_batch is an offline CLI that
// serves no routes (VLLM.md §1.3). SGLang has neither (SGLANG.md §1.3). So a
// batch endpoint that forwarded to an upstream batch endpoint would work on
// approximately no backend dorang supports. The scheduler here is the
// foundation, not a fallback; a native upstream batch path is an optional
// accelerator enabled per provider (DESIGN §11.1), and note that the fan-out
// shape conflicts with this package's central assumption that partial failure is
// normal.
//
// # Pipeline
//
//	upload → validate (JSONL, unique custom ids, known models, size and line ceilings)
//	create → validating → queued → in_progress → finalizing → completed
//	  rows grouped by prefix hash (§7.4b) so cache-adjacent work runs together
//	  each row is an ordinary request at batch priority (§7.5)
//	  everything flows through §5, so batch cannot exceed configured capacity
//	  retries with backoff on retryable statuses; partial failure is expected
//	complete → output JSONL + error JSONL
//	cancel → cancelling → in-flight drains → cancelled, partial results preserved
//
// # Interactive traffic is protected on every axis
//
// Every capacity request this package makes sets [CapacityRequest.Batch], and it
// is set in exactly one place (Service.capacityRequest) so that it cannot be
// forgotten on some path. That single flag is the whole of §11.1's protection:
// the broker applies its interactive reserve to *every* axis a batch request
// touches, not just the credential one. Revision 1 of the design capped batch at
// a share of credential concurrency, under which batch could take a model's
// entire limit while staying comfortably under its credential share and starve
// interactive traffic for that model completely. TestModelAxisContention asserts
// the model axis specifically, and its subtests demonstrate that the equivalent
// credential-axis assertion passes while that defect is live.
//
// # Cancel does not discard finished work
//
// Cancelling stops dispatch of new rows and waits for in-flight rows to finish;
// it never aborts a row that is already executing and never drops a row that
// already completed. The output and error files of a cancelled batch contain
// every row that finished, because the caller already paid for those tokens.
// A batch cancelled while still queued never runs a row and reaches cancelled
// synchronously.
//
// # Partial failure is not batch failure
//
// A row that fails terminally goes to the error file and is counted in
// request_counts.failed. The batch is still completed. Only a failure of the
// batch itself — validation, or a store that cannot record results — is failed.
//
// # Restart
//
// Row state is persisted as each row reaches a terminal state, so [Service.Recover]
// resumes a batch from where it stopped rather than re-running it. Across a
// graceful [Service.Close] this is exactly-once: Close drains in-flight rows and
// their results are recorded before it returns. Across a crash it is
// at-least-once, which is the same guarantee a retry already has.
//
// # Dependencies
//
// This package deliberately imports none of internal/router, internal/capacity
// or internal/store. It declares the narrow interfaces it consumes — [Executor],
// [Reserver], [Store], [Blobs], [ModelResolver], [Clock] — so that the wiring
// layer adapts them and this package can be tested without any of them. The one
// internal import is internal/prefix, whose hash chain is used as a grouping key.
package batch

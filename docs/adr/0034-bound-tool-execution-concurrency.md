# 34. Bound tool execution concurrency

Status: Accepted

Date: 2026-09-14

Deciders: Guy Grigsby

## Context

ADR 0028 made tool calls from one assistant message concurrent so a blocked permission or slow
call would not serialize unrelated work. Its implementation starts one goroutine per provider
tool-use block and leaves cardinality uncapped. A provider can therefore make the daemon allocate an
unbounded outcomes slice, goroutines, Gate requests and adapter callbacks from one response.

The concurrency requirement remains. The missing constraint is a resource boundary shared across
Turns rather than a return to serial execution.

## Decision

Supersede ADR 0028 only where it permits unbounded tool execution. Refuse a provider assistant
message that would raise its active Turn above 64 total tool-use blocks before appending the
assistant Entry or submitting any call. Refuse an id already used by any earlier tool call in the
same active Turn at the same boundary. Treat either response as a provider failure.

One Server-owned scheduler runs 64 workers through two bounded admission lanes totaling 64 queued
jobs. Forty-eight general workers may serve root or child Session jobs. Sixteen child-reserved
workers serve only child Session jobs. The root queue holds 48 jobs and the child queue holds 16.
General workers help drain child work when available, but root work can never occupy the reserved
workers. Since a child Session cannot invoke `agent`, 48 root `agent` calls may wait for child Turns
while at least 16 workers remain able to execute those child jobs.

Turns submit calls with cancellation-aware backpressure and do not start a goroutine per call.
Workers remain concurrent across Sessions and append results as they finish. Cancellation covers
both queues plus running and not-yet-submitted calls and prevents a success result after the Turn
is cut. Server shutdown closes scheduler admission, cancels every job and joins all workers before
teardown completes.

## Consequences

- One provider response has a fixed tool, goroutine and permission fan-out bound.
- Independent calls still fill all eligible workers, and general workers help child work when they
  are free.
- Root execution concurrency is capped at 48 to preserve structural progress for subagent work.
  Fair scheduling within either lane is not promised by this decision.
- A full root response of `agent` calls cannot deadlock the Server-wide scheduler while its children
  need tools.
- A model response that would raise its Turn above 64 tool calls fails rather than partially
  executing an ambiguous prefix.
- A provider cannot reuse a tool-use id within one Turn, so that id remains part of a stable
  permission-question identity across successive assistant responses.

# 31. Explicit terminal proof completes daemon shutdown

- Status: Accepted
- Date: 2026-09-14
- Deciders: Guy Grigsby
- Supersedes: ADR 0030 decisions 2 through 4 where bare connection EOF was treated as cleanup proof

## Context

ADR 0030 bound daemon shutdown to an authenticated Server connection and removed unsafe pid
signaling. Its first completion contract treated EOF on that connection as proof that runtime
cleanup finished.

EOF cannot carry that meaning. A process crash, transport loss and a deliberate close after a
cleanup error all produce the same observation. Some plugins also own internal shutdown
budgets, so an unbounded outer context does not make every plugin close successful. Starting a
replacement after bare EOF can overlap abandoned plugin work with the new daemon.

The Server already owns an ordered response pump and a process-lifetime instance id. It can
state successful completion on that exact connection before closing it.

## Decision

1. Keep `server.shutdown {}` as the authenticated request. Reserve shutdown immediately so no
   new work is admitted, but leave public Server state `running` until the success response is
   physically written. A failed response releases the reservation.

2. Retain the accepted control connection and its ordered writer through cleanup. After every
   runtime resource and the listener close successfully, transition the Server to `stopped`,
   send `server.stopped {instance_id, state: "stopped"}`, then close the connection.

3. On cleanup failure, send no terminal success notification. Release the control connection
   without proof and exit unsuccessfully. Bare EOF is always failure at the client boundary.

4. `rudy bridge --stop` and `rudy hosts install --force` require a `server.stopped`
   notification for the same instance followed by EOF. Only then may a replacement start.

## Consequences

- Cleanup success, transport loss and process death are distinguishable without a process id.
- A client that receives shutdown acceptance but no terminal proof fails closed.
- The control response pump outlives ordinary connection cancellation until success or failure
  is known. Both terminal delivery and the initial response flush remain bounded.
- The Server state machine stays one-way. Tentative admission fencing is not a lifecycle state.

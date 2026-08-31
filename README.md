# Global Build (retired)

Global Build was retired on 2026-08-31. It is no longer an execution runtime,
orchestrator, worker, scheduler, publication path, or compatibility layer.

The normal flow is now direct:

```text
Problem Framer -> appropriate executor -> target repository authority
```

This repository intentionally contains no runnable implementation, embedded
agent prompt, runtime state machine, CI job, or installer. Do not restore or
reinstall Global Build as an intermediate execution layer.

No utility package was retained here: every former Go package was internal to
this module and had no consumer outside Global Build. Independently consumed
repository-local Git, test, watch, and concurrency primitives remain owned by
their consuming repositories.

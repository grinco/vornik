# Timeouts on your own hardware

Vornik's time budgets — how long a step may run, how long a task lease is held —
are absolute wall-clock values. They were chosen on particular hardware. Move to
hardware that generates tokens at a different speed and those numbers stop
meaning what they meant.

The gap is not small. Two self-hosted endpoints measured on the same day:

| | marginal decode | per-request overhead |
|---|---:|---:|
| a fast GPU host | 206 tok/s | 58 ms |
| a modest local box | 12 tok/s | 499 ms |

Seventeen times. Replaying real recorded step shapes at the slower rate puts 16%
of steps past the default 5-minute lease timeout, where the fast host had none.
Those tasks are re-leased mid-flight and their containers die — and nothing in
the logs says "your hardware is slower than this budget assumed".

## Measure first

```sh
vornikctl profile
```

With no arguments it reads the daemon's own database and fits every model that
has recent work:

```
MODEL                      STEPS  DECODE tok/s  ±   PER TOOL CALL  FIXED
Qwen/Qwen3-Coder-Next-FP8    689           215  6%          0.80s   -0.0s
```

Three separate numbers, deliberately:

- **DECODE** is the model's own rate. This is the one a timeout should scale on.
- **PER TOOL CALL** is what your tools cost. A rising value here is a tool
  problem, and must not buy the model more time.
- **±** is how uncertain the decode rate is. Above 30% the command refuses to
  report a rate at all: a figure that loose is a range, not a number.

The naive measure — tokens divided by step duration — folds all three together,
and would make a deployment look slower simply for having slow tools.

### A machine with no history

A fresh install has nothing to fit, which is exactly when you want to know
whether the hardware is usable. Probe the endpoint directly:

```sh
vornikctl profile \
  --probe-endpoint http://your-host:8000/v1 \
  --probe-model your-model \
  --suggest-config
```

This needs no database and no prior work. It measures decode as the **slope**
between a short and a long generation, so per-request overhead cancels instead of
dragging the rate down.

Probe and fit measure different things and are never averaged. The probe is what
the hardware can do; the fit is what it does under your real workload. When both
exist the command prints them together, and the **gap between them is your
contention** — a scheduling story, not a model one.

## Then enable

```sh
vornikctl profile --suggest-config
```

emits a block to paste into the config the daemon actually reads (confirm with
`vornikctl config show`):

```yaml
scheduler:
  speed_aware_timeouts:
    enabled: true
    reference_tokens_per_sec: 215
    observed_tokens_per_sec: 215
    min_factor: 0.5
    max_factor: 8.0
```

`reference_tokens_per_sec` is the only value needing judgement. Read it as a
claim you are making: *"the timeouts in this deployment work as they are, on
hardware that decodes at this rate."* If that is true of the machine you just
measured, use its number. Every slower host then scales against a baseline that
demonstrably worked.

It cannot be inferred for you. If each deployment used its own measurement as its
own reference, the factor would be 1.0 everywhere and the feature would never do
anything.

The feature is **off by default**, and off is byte-identical to not having it.

### When hardware is too slow

`max_factor` is deliberately lower than the slowest hardware would ask for. A 12
tok/s box against a 215 tok/s reference wants roughly 18x; the ceiling stops at
8x, logs a warning, and a step that then times out fails as `hardware_too_slow`
rather than as a generic timeout.

That is on purpose. The honest answer at 18x is that the hardware cannot run the
workload in this shape — not a two-hour step timeout arrived at by arithmetic.
Raise `max_factor` if you disagree; it is your call, made knowingly.

## Budgets this does not scale

Build and test tools are **compute-bound, not decode-bound** — a `go build` does
not get faster because the model does. They have their own knob:

```sh
VORNIK_TOOL_TIMEOUT_FACTOR=2.5    # in the agent container's environment
```

It is a factor rather than absolute overrides so the deliberate ratios survive: a
Rust build stays longer than a typecheck.

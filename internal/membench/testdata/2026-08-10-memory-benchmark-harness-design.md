# Memory Benchmark Harness + Chunk Event-Time (fixture excerpt)

No backfill. Setting event_time = created_at on existing rows would assert a
fact we do not have. The read path expresses the fallback explicitly with
COALESCE(event_time, created_at), which makes the change strictly widening.

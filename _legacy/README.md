# Frozen embedded-inference evidence

`embedded/` retains the source and tests removed from the executable runtime.
They describe the pre-separation implementation at Git commit
`36de3b92d1169207b043f12244c16502b2de0a27`. The underscore prefix excludes this
archive from Go package traversal; the container build also excludes it.
These files are evidence, not supported configuration or runnable v5 code.
Use that historical revision in an isolated checkout to reproduce old tests.

Historical SQL migrations, durable captures, dispatch fences, committed results,
budgets and catalogue recovery snapshots remain intact. Migration 61 freezes
unfinished legacy queue work with before-images. It does not contact a provider,
replay a capture or recreate an inference result. See the v5 migration guide.

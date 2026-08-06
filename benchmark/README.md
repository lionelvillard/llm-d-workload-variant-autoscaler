# Benchmark Specs

This directory contains [llm-d-benchmark](https://github.com/llm-d/llm-d-benchmark) specifications and scenarios for benchmarking this repository's autoscaler.

## Prerequisites

Clone `llm-d-benchmark` as a sibling of this repo and install the CLI:

```bash
git clone https://github.com/llm-d/llm-d-benchmark.git ../llm-d-benchmark
cd ../llm-d-benchmark && ./install.sh
```

The CLI is then available at `../llm-d-benchmark/.venv/bin/llmdbenchmark` from the root of this repo.

## Directory layout

```
benchmark/
├── config/
│   ├── specification/        # Jinja2 spec templates (.yaml.j2) — point llmdbenchmark at these
│   ├── scenarios/            # Scenario YAMLs consumed by each spec
│   └── templates/
│       └── values/
│           └── defaults.yaml # Default values merged into every scenario
```

## Running a benchmark

### Running the CLI directly

For finer control you can invoke `llmdbenchmark` directly from the repo root:

```bash
LLMDBENCH=../llm-d-benchmark/.venv/bin/llmdbenchmark

# Standup
$LLMDBENCH \
  --spec benchmark/config/specification/pd-disaggregation-sim.yaml.j2 \
  standup -p <namespace>

# Run
$LLMDBENCH \
  --spec benchmark/config/specification/pd-disaggregation-sim.yaml.j2 \
  run -p <namespace> -l guidellm -w prefill_heavy.yaml

# Teardown
$LLMDBENCH \
  --spec benchmark/config/specification/pd-disaggregation-sim.yaml.j2 \
  --workspace . \
  --base-dir ../llm-d-benchmark \
  teardown -p <namespace>
```

Use `--dry-run` / `-n` to preview what would be applied without touching the cluster.

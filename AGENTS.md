# Claude Code Assistant Guidelines

## Go Code Style

- Follow the standard Go code style and conventions. Use `gofmt` for formatting and adhere to idiomatic Go practices.
- Follow best practices from the [Effective Go](https://go.dev/doc/effective_go) guide:

### Naming Conventions
- Use **MixedCaps** or **mixedCaps** rather than underscores for multi-word names
- Package names should be short, lowercase, single-word names
- Getters don't use "Get" prefix (use `obj.Name()` not `obj.GetName()`)
- Interface names use "-er" suffix for single-method interfaces (e.g., `Reader`, `Writer`)

### Formatting
- Use `gofmt` for consistent formatting (tabs for indentation, spaces for alignment)
- Line length: no strict limit, but keep lines reasonable
- Group related declarations together

### Error Handling
- Return errors as the last return value
- Check errors immediately after the call
- Provide context with `fmt.Errorf` and error wrapping

### Logging
- Use `ctrl.Log` for structured logging
- Keep log fields consistent and meaningful
- Avoid logging sensitive data

### Documentation
- Every exported name should have a doc comment
- Start comments with the name being described
- Use complete sentences

### Concurrency
- Share memory by communicating; don't communicate by sharing memory
- Use channels to orchestrate goroutines
- Always handle goroutine cleanup and cancellation properly

### Project Structure
- Keep packages focused and cohesive
- Avoid circular dependencies
- Place tests in `*_test.go` files

## Documentation

Prefer placing documentation in the `docs/` directory.

There are 3 main types of documentation targeting different audiences:

1. **Developer Documentation** - For contributors and maintainers of this project
   - Architecture decisions
   - Development setup and workflow
   - Contributing guidelines
   - usually in the `docs/developer-guide/` subdirectory

2. **Administrator Documentation** - For operators deploying and managing the autoscaler controller
   - Installation and configuration
   - Deployment guidelines
   - Monitoring and troubleshooting
   - usually located under the `docs/user-guide/` directory (for example, in an admin-focused subdirectory)

3. **End-User Documentation** - For application developers creating applications that use the autoscaler
   - Usage guides and examples
   - API reference
   - Best practices and common patterns
   - usually located under the `docs/user-guide/` directory (for example, in an end-user-focused subdirectory)

## E2E Testing

- use make targets for running e2e tests (e.g., `make test-e2e`) and document the process in `docs/developer-guide/testing.md`
- use `make test` for unit tests
- **Never use images from docker.io in e2e tests.** All container images must use fully-qualified registry paths (e.g., `registry.k8s.io/`, `quay.io/`, or a private registry). Do not rely on Docker Hub as a default registry.

## CLI Tools

Use focused agent skills for CLI configuration and migration guidance.

- EPP CLI skill: `.github/agents/epp-cli-usage.agent.md`
- Inference simulator CLI skill: `.github/agents/inference-simulator-cli-usage.agent.md`

Guidance for these skills is main-branch first, with compatibility notes for EPP v0.5.1 and inference simulator v0.7.1 where migration pitfalls are common.

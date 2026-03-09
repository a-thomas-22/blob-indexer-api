# Contributing to Blob Indexer

Thanks for your interest in contributing! Here's how to get started.

## Development Setup

1. **Prerequisites**: Go 1.24+, PostgreSQL, Make
2. Clone the repo and run `make build` to verify everything compiles
3. Copy `config.yaml` and configure your local database and RPC URLs
4. Run `make db-migrate` to set up the database schema
5. Run `make seed-data` to populate test data

## Making Changes

1. Create a branch from `main`
2. Make your changes
3. Run checks before pushing:
   ```bash
   make fmt && make lint && make test
   ```
   Or run the full CI suite locally:
   ```bash
   make ci
   ```
4. Open a pull request

## Pull Request Guidelines

- **PR titles must follow [Conventional Commits](https://www.conventionalcommits.org/)**: `feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:`, etc.
- Keep PRs focused — one feature or fix per PR
- Include tests for new functionality
- Maintain the 90% code coverage threshold

## Code Style

- Run `make fmt` to auto-format (gofmt + goimports)
- Follow existing patterns in the codebase
- Use the structured logger (`logger.Info`, `logger.Error`, etc.) instead of `fmt.Print`
- Use `respondJSON`/`respondError` helpers in API handlers

## Reporting Issues

- Use GitHub Issues to report bugs or request features
- Include steps to reproduce for bug reports
- Check existing issues before opening a new one

## License

By contributing, you agree that your contributions will be licensed under the MIT License.

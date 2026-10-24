# Changelog

All notable changes to **puffball** (`puff`) are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- **`puff version`** prints the version, commit and build date, and checks
  GitHub for a newer release; `--json` prints the same fields as JSON.
- **Global flags** shared with the other spore.host tools: `--profile` and
  `--region` (resolved flag > `SPORE_*`/`AWS_*` env > `~/.config/spore/config.toml`),
  `--json`, `--verbose`, `--lang`, `--no-emoji`, `--accessibility`.
- **Exit codes** 0 (success), 1 (error), 2 (usage error).

[Unreleased]: https://github.com/spore-host/puffball/commits/main

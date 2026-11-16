---
paths:
  - "**/*.py"
  - "**/*.pyi"
---
# Python Security

> This file extends [standards/security.md](../../standards/security.md) with Python specific content.

- Static analysis with **bandit**: `bandit -r src/`.
- Read secrets with `os.environ["NAME"]` (raises when missing), never `.get` with a literal default.
- `subprocess.run([...], check=True)` with list args; never `os.system` or `shell=True` on interpolated input.

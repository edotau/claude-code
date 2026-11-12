---
paths:
  - "**/*.py"
  - "**/*.pyi"
---
# Python Security

> This file extends [standards/security.md](../../standards/security.md) with Python specific content.

## Prohibited vs Correct

```python
API_KEY = "sk-abc123..."              # WRONG — hardcoded secret
API_KEY = os.environ["API_KEY"]      # CORRECT — from environment; raises if missing

os.system(f"git clone {url}")                      # WRONG — shell injection
subprocess.run(["git", "clone", url], check=True)  # CORRECT — list args, no shell
```

## Security Scanning

- Use **bandit** for static security analysis:

  ```bash
  bandit -r src/
  ```

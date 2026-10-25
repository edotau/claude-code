---
paths:
  - "**/*.py"
  - "**/*.pyi"
---
# Python Security

> This file extends [standards/security.md](../../standards/security.md) with Python specific content.

## Secret Management

```python
import os
from dotenv import load_dotenv

load_dotenv()

api_key = os.environ["OPENAI_API_KEY"]  # Raises KeyError if missing
```

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

## Reference

The `evaluate-code` skill's Security lens carries the OWASP checklist and dependency-audit procedure.

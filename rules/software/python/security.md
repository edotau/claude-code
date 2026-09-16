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

## Security Scanning

- Use **bandit** for static security analysis:
  ```bash
  bandit -r src/
  ```

## Reference

The `security-reviewer` skill carries the OWASP checklist and dependency-audit procedure.

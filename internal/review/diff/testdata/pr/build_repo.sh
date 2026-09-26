#!/bin/bash
# Deterministic git fixture for pr_analyzer tests — fixed dates/author so commit
# hashes are reproducible; usage: build_repo.sh <dest-dir>
set -euo pipefail
DEST="$1"
rm -rf "$DEST"
mkdir -p "$DEST"
cd "$DEST"

export GIT_AUTHOR_NAME="Test Author"
export GIT_AUTHOR_EMAIL="test@example.com"
export GIT_COMMITTER_NAME="Test Author"
export GIT_COMMITTER_EMAIL="test@example.com"
export GIT_AUTHOR_DATE="2026-01-01T00:00:00"
export GIT_COMMITTER_DATE="2026-01-01T00:00:00"

git init -q -b main
git config user.name "Test Author"
git config user.email "test@example.com"

mkdir -p src
cat > README.md <<'EOF'
# fixture repo
EOF
cat > src/app.js <<'EOF'
function run() {
  console.log("start")
}
EOF
cat > src/safe.py <<'EOF'
def add(a, b):
    return a + b
EOF
git add README.md src/app.js src/safe.py
git commit -q -m "feat: initial fixture repo"

git checkout -q -b feature
cat > src/auth.js <<'EOF'
function login(user) {
  const token = "hardcoded-secret-token"
  return token
}
EOF
cat >> src/app.js <<'EOF'
console.log("debug line")
// TODO: clean this up
EOF
export GIT_AUTHOR_DATE="2026-01-01T00:05:00"
export GIT_COMMITTER_DATE="2026-01-01T00:05:00"
git add src/auth.js src/app.js
git commit -q -m "add auth"

export GIT_AUTHOR_DATE="2026-01-01T00:10:00"
export GIT_COMMITTER_DATE="2026-01-01T00:10:00"
cat >> src/auth.js <<'EOF'
debugger
EOF
git add src/auth.js
git commit -q -m "second commit without conventional prefix that is definitely way too long to pass the length check"

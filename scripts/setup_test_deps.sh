#!/usr/bin/env bash
# Installs what `make test-integration` needs: a Java runtime and DynamoDB Local.
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DDB_DIR="${DYNAMODB_LOCAL_DIR:-$DIR/.dynamodb-local}"

if ! java -version >/dev/null 2>&1 && [ ! -x /opt/homebrew/opt/openjdk/bin/java ] && [ -z "${JAVA_HOME:-}" ]; then
  if command -v brew >/dev/null 2>&1; then
    brew install openjdk
  else
    echo "Install a Java 17+ runtime and set JAVA_HOME" >&2
    exit 1
  fi
fi

if [ ! -f "$DDB_DIR/DynamoDBLocal.jar" ]; then
  mkdir -p "$DDB_DIR"
  curl -sfL https://d1ni2b6xgvw0s0.cloudfront.net/v2.x/dynamodb_local_latest.tar.gz | tar -xz -C "$DDB_DIR"
fi

echo "DynamoDB Local ready in $DDB_DIR"

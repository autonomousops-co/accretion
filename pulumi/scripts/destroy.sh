#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PULUMI_DIR="$(dirname "$SCRIPT_DIR")"

STACK="${1:-dev}"

echo "WARNING: This will destroy all accretion-api infrastructure for stack: $STACK"
echo ""

cd "$PULUMI_DIR"

pulumi stack select "$STACK"

echo "Resources to be destroyed:"
pulumi preview --diff

echo ""
read -p "Are you sure you want to destroy all resources? (type 'yes' to confirm) " CONFIRM
if [ "$CONFIRM" = "yes" ]; then
    pulumi destroy --yes
    echo ""
    echo "Stack $STACK destroyed."
else
    echo "Destroy cancelled."
fi

#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PULUMI_DIR="$(dirname "$SCRIPT_DIR")"

# Default stack
STACK="${1:-dev}"

echo "Deploying accretion-api infrastructure (stack: $STACK)"
echo ""

# pulumi must run where Pulumi.yaml is. Note the chart paths no longer depend on
# this: they are absolute, resolved from the repository root, because the old
# relative "./charts/<name>" resolved against this directory and there is no
# pulumi/charts, so no release could ever render.
cd "$PULUMI_DIR"

# Pulumi.yaml sets runtime.options.binary, so pulumi executes a prebuilt binary
# instead of compiling the program itself. Without this step preview and up run
# whatever was built last -- or nothing at all on a fresh checkout.
go build -o accretion-infrastructure .

# Select or create stack
if pulumi stack ls 2>/dev/null | grep -q "^$STACK"; then
    pulumi stack select "$STACK"
else
    pulumi stack init "$STACK" --secrets-provider=default
    # Copy stack config
    if [ -f "stacks/Pulumi.$STACK.yaml" ]; then
        cp "stacks/Pulumi.$STACK.yaml" "Pulumi.$STACK.yaml"
        echo "Applied stack config from stacks/Pulumi.$STACK.yaml"
    else
        echo "Warning: No stack config found at stacks/Pulumi.$STACK.yaml"
        echo "You may need to configure the stack manually with 'pulumi config set'"
    fi
fi

# Preview first
echo ""
echo "Previewing changes..."
pulumi preview

echo ""
read -p "Apply changes? (y/N) " -n 1 -r
echo
if [[ $REPLY =~ ^[Yy]$ ]]; then
    pulumi up --yes
    echo ""
    echo "Deployment complete!"
    echo ""
    pulumi stack output
else
    echo "Deployment cancelled."
fi

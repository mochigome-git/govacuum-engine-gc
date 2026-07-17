#!/bin/bash
set -e

# ─── Load .env ───────────────────────────────────────────────────────────────
if [ ! -f .env ]; then
  echo "❌ .env file not found. Run this script from the project root."
  exit 1
fi

set -a
source .env
set +a

# ─── Validate required vars ───────────────────────────────────────────────────
: "${APP_VERSION:?❌ APP_VERSION is not set in .env}"
: "${DOCKER_TAG:?❌ DOCKER_TAG is not set in .env}"
: "${ECR_REPO_NAME:?❌ ECR_REPO_NAME is not set in .env}"
: "${ECR_REGION:?❌ ECR_REGION is not set in .env}"
: "${ECR_PUBLIC_ALIAS:?❌ ECR_PUBLIC_ALIAS is not set in .env}"
: "${ECR_PUBLIC_REPO_NAME:?❌ ECR_PUBLIC_REPO_NAME is not set in .env}"

ECR_PUBLIC_REGION="us-east-1"
AWS_ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)

PRIVATE_IMAGE="${AWS_ACCOUNT_ID}.dkr.ecr.${ECR_REGION}.amazonaws.com/${ECR_REPO_NAME}:v${APP_VERSION}"
PUBLIC_IMAGE="public.ecr.aws/${ECR_PUBLIC_ALIAS}/${ECR_PUBLIC_REPO_NAME}:v${APP_VERSION}"
PUBLIC_IMAGE_LATEST="public.ecr.aws/${ECR_PUBLIC_ALIAS}/${ECR_PUBLIC_REPO_NAME}:latest"

echo ""
echo "  Private : ${PRIVATE_IMAGE}"
echo "  Public  : ${PUBLIC_IMAGE}"
echo "  Public  : ${PUBLIC_IMAGE_LATEST}"
echo ""

# ─── Login to Private ECR (read access to source manifest) ───────────────────
echo "🔐 Logging into Private ECR..."
aws ecr get-login-password --region "${ECR_REGION}" \
  | docker login --username AWS --password-stdin \
    "${AWS_ACCOUNT_ID}.dkr.ecr.${ECR_REGION}.amazonaws.com"

# ─── Verify the source image exists and check what platforms it has ──────────
echo "🔍 Inspecting source manifest..."
if ! docker buildx imagetools inspect "${PRIVATE_IMAGE}" > /dev/null 2>&1; then
  echo "❌ Could not find or inspect ${PRIVATE_IMAGE} in private ECR."
  echo "   Make sure it was built and pushed with 'docker buildx build --platform ... --push'."
  exit 1
fi
docker buildx imagetools inspect "${PRIVATE_IMAGE}"

# ─── Login to Public ECR (write access to destination) ───────────────────────
echo ""
echo "🔐 Logging into Public ECR..."
aws ecr-public get-login-password --region "${ECR_PUBLIC_REGION}" \
  | docker login --username AWS --password-stdin public.ecr.aws

# ─── Copy multi-platform manifest list directly, registry-to-registry ────────
# NOTE: We intentionally do NOT docker pull/tag/push here.
# `docker pull` only fetches the single-platform image matching this host's
# architecture, and `docker push` from a local tag only pushes that one
# flattened image — this silently drops the other platforms from a multi-arch
# manifest list. `imagetools create` copies the full manifest list (and every
# platform image it references) directly between registries without ever
# materializing a single-arch image locally.
echo ""
echo "📤 Copying multi-arch image to Public ECR..."
docker buildx imagetools create \
  -t "${PUBLIC_IMAGE}" \
  -t "${PUBLIC_IMAGE_LATEST}" \
  "${PRIVATE_IMAGE}"

# ─── Verify the pushed public image is still multi-arch ──────────────────────
echo ""
echo "🔍 Verifying public manifest..."
docker buildx imagetools inspect "${PUBLIC_IMAGE}"

echo ""
echo "✅ Done! public.ecr.aws/${ECR_PUBLIC_ALIAS}/${ECR_PUBLIC_REPO_NAME}:v${APP_VERSION}"
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

PRIVATE_IMAGE="${AWS_ACCOUNT_ID}.dkr.ecr.${ECR_REGION}.amazonaws.com/${ECR_REPO_NAME}:${APP_VERSION}"
PUBLIC_IMAGE="public.ecr.aws/${ECR_PUBLIC_ALIAS}/${ECR_PUBLIC_REPO_NAME}:${APP_VERSION}"
PUBLIC_IMAGE_LATEST="public.ecr.aws/${ECR_PUBLIC_ALIAS}/${ECR_PUBLIC_REPO_NAME}:latest"
LOCAL_IMAGE="${DOCKER_TAG}:${APP_VERSION}"

echo ""
echo "  Private : ${PRIVATE_IMAGE}"
echo "  Public  : ${PUBLIC_IMAGE}"
echo ""

# ─── Check if image exists locally, pull from private ECR if not ──────────────
if docker image inspect "${LOCAL_IMAGE}" > /dev/null 2>&1; then
  echo "✅ Image found locally, skipping pull."
else
  echo "📥 Image not found locally, pulling from private ECR..."
  aws ecr get-login-password --region "${ECR_REGION}" \
    | docker login --username AWS --password-stdin \
      "${AWS_ACCOUNT_ID}.dkr.ecr.${ECR_REGION}.amazonaws.com"
  docker pull "${PRIVATE_IMAGE}"
  docker tag "${PRIVATE_IMAGE}" "${LOCAL_IMAGE}"
fi

# ─── Login to Public ECR ─────────────────────────────────────────────────────
echo "🔐 Logging into Public ECR..."
aws ecr-public get-login-password --region "${ECR_PUBLIC_REGION}" \
  | docker login --username AWS --password-stdin public.ecr.aws

# ─── Tag & Push ───────────────────────────────────────────────────────────────
echo "🏷️  Tagging..."
docker tag "${LOCAL_IMAGE}" "${PUBLIC_IMAGE}"
docker tag "${LOCAL_IMAGE}" "${PUBLIC_IMAGE_LATEST}"

echo "📤 Pushing..."
docker push "${PUBLIC_IMAGE}"
docker push "${PUBLIC_IMAGE_LATEST}"

echo ""
echo "✅ Done! public.ecr.aws/${ECR_PUBLIC_ALIAS}/${ECR_PUBLIC_REPO_NAME}:${APP_VERSION}"
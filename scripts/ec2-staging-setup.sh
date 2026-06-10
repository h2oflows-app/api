#!/usr/bin/env bash
# ec2-staging-setup.sh — One-time prep of the EC2 host for the staging stack.
# Run ON the box: cd /home/ubuntu/h2oflows && ./scripts/ec2-staging-setup.sh
set -euo pipefail

echo "▶ [1/3] 2GB swap (build/runtime safety net)..."
if ! swapon --show | grep -q /swapfile; then
  sudo fallocate -l 2G /swapfile
  sudo chmod 600 /swapfile
  sudo mkswap /swapfile
  sudo swapon /swapfile
  grep -q '/swapfile' /etc/fstab || echo '/swapfile none swap sw 0 0' | sudo tee -a /etc/fstab
  echo "  swap on."
else
  echo "  swap already present."
fi

echo "▶ [2/3] Shared docker network h2oedge..."
docker network inspect h2oedge >/dev/null 2>&1 || docker network create h2oedge
echo "  ok."

echo "▶ [3/3] GHCR pull access for api-stg image:"
echo "  The staging image ghcr.io/h2oflows-app/api:staging is private by default."
echo "  EITHER set the package visibility to Public in GitHub (Packages > api > settings),"
echo "  OR docker-login on this host with a read:packages PAT:"
echo "     echo <PAT> | docker login ghcr.io -u <github-user> --password-stdin"
echo ""
echo "NOTE — EBS volume grow (root is ~78% full) is a separate AWS-console step:"
echo "  1. AWS console: EC2 > Volumes > modify volume size (e.g. 19 -> 30 GiB)."
echo "  2. On the box: sudo growpart /dev/<root-disk> 1 && sudo resize2fs /dev/root"
echo "     (find the device with: lsblk)"
echo ""
echo "✓ Host prep done. Next: copy .env.staging.example -> .env.staging, fill it,"
echo "  then the deploy-staging GitHub Action (label a PR 'staging') brings the stack up."

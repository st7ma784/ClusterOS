# Quick Setup: GitHub Actions & Monitoring

**5-minute guide to enable automated builds and monitoring**

## Enable GitHub Actions

### 1. Enable Workflows

```bash
# Push workflows to GitHub
git add .github/workflows/
git commit -m "Add GitHub Actions workflows"
git push origin main
```

### 2. Configure Repository Settings

1. Go to **Settings → Actions → General**
2. Set **Workflow permissions** to "Read and write permissions"
3. Save

### 3. Enable GitHub Pages

1. Go to **Settings → Pages**
2. Source: **GitHub Actions**
3. Save

### 4. Trigger First Build

```bash
# Create and push a release tag
git tag -a v0.1.0 -m "Initial release"
git push origin v0.1.0

# Monitor build:
# Go to Actions tab in GitHub
# Watch the "Build OS Images and Release" workflow
```

### 5. Access Documentation

After docs deploy:
- Visit: `https://<username>.github.io/<repo-name>/`
- View all guides with beautiful formatting
- Mobile-friendly design

## Deploy Monitoring Stack

### 1. Install Docker

```bash
sudo apt-get update
sudo apt-get install -y docker.io docker-compose
sudo usermod -aG docker $USER
# Log out and back in
```

### 2. Create Monitoring Directory

```bash
mkdir -p ~/monitoring
cd ~/monitoring
```

### 3. Download Monitoring Configs

Create `docker-compose.yml` from `docs/MONITORING_OPERATIONS.md` or:

```bash
# If you have the repo locally
cp /path/to/ClusterOS/docs/monitoring-configs/* ~/monitoring/
```

### 4. Create Configuration Files

```bash
# Create directories
mkdir -p prometheus/alerts
mkdir -p alertmanager
mkdir -p grafana/provisioning/datasources

# Create prometheus.yml (see MONITORING_OPERATIONS.md)
# Create alerts/*.yml (see MONITORING_OPERATIONS.md)
# Create alertmanager.yml (see MONITORING_OPERATIONS.md)
```

### 5. Start Stack

```bash
docker-compose up -d

# Verify
docker-compose ps

# View logs
docker-compose logs -f
```

### 6. Install Exporters on Cluster Nodes

On each node:

```bash
# Node exporter
sudo apt-get install -y prometheus-node-exporter
sudo systemctl enable --now prometheus-node-exporter

# Access from monitoring host
curl http://node1:9100/metrics
```

### 7. Access Dashboards

```bash
# Prometheus
open http://localhost:9090

# Grafana (default: admin/admin)
open http://localhost:3000

# AlertManager
open http://localhost:9093
```

### 8. Import Dashboards in Grafana

1. Login to Grafana
2. Click **+** → **Import**
3. Import these dashboards:
   - **1860** - Node Exporter Full
   - **7249** - Kubernetes Cluster
   - **4323** - SLURM Dashboard

## Quick Tests

### Test GitHub Actions

```bash
# Create a test tag
git tag -a v0.1.1 -m "Test release"
git push origin v0.1.1

# Check Actions tab
# Artifacts will appear in Releases
```

### Test Documentation Site

```bash
# Make a doc change
echo "# Test" >> docs/TEST.md
git add docs/TEST.md
git commit -m "Test docs"
git push origin main

# Check deployment
# Visit https://<username>.github.io/<repo-name>/
```

### Test Monitoring

```bash
# Query Prometheus
curl "http://localhost:9090/api/v1/query?query=up"

# Check targets
curl http://localhost:9090/api/v1/targets | jq

# View Grafana dashboards
# Navigate to http://localhost:3000
```

## Common Commands

### GitHub Actions

```bash
# Trigger manual build
# Go to Actions → Build OS Images → Run workflow

# View workflow runs
gh run list  # Requires GitHub CLI

# Download artifacts
gh run download <run-id>
```

### Monitoring

```bash
# Check services
docker-compose ps

# View logs
docker-compose logs prometheus
docker-compose logs grafana

# Restart service
docker-compose restart prometheus

# Stop all
docker-compose down

# Start again
docker-compose up -d
```

### Cluster Health

```bash
# SLURM
sinfo
squeue

# Kubernetes
sudo k3s kubectl get nodes
sudo k3s kubectl get pods -A

# Cluster ring
sudo serf members
sudo wg show
```

## Troubleshooting

### GitHub Actions Not Running

```bash
# Check if workflows are enabled
# Settings → Actions → Allow all actions

# Check permissions
# Settings → Actions → Workflow permissions
```

### Prometheus Not Scraping

```bash
# Check targets
curl http://localhost:9090/api/v1/targets

# Check connectivity from container
docker exec prometheus wget -qO- http://node1:9100/metrics

# View logs
docker logs prometheus
```

### Grafana No Data

```bash
# Check data source
curl http://localhost:3000/api/datasources

# Test Prometheus connection
curl "http://localhost:9090/api/v1/query?query=up"

# Verify time range in Grafana
# Check upper-right time selector
```

## Documentation Index

### Setup Guides
- **This File** - Quick setup
- **GITHUB_ACTIONS_SUMMARY.md** - Complete Actions guide
- **docs/MONITORING_OPERATIONS.md** - Complete monitoring setup

### Monitoring Guides
- **docs/MONITORING_SLURM.md** - SLURM scheduler monitoring
- **docs/MONITORING_KUBERNETES.md** - K8s cluster monitoring
- **docs/MONITORING_CLUSTER.md** - Cluster ring & nodes
- **docs/MONITORING_OPERATIONS.md** - Unified dashboard

### Other Guides
- **PACKER_QEMU_QUICKSTART.md** - Build images
- **GETTING_STARTED.md** - Cluster setup
- **docs/VM_TESTING.md** - Testing with VMs
- **docs/DEPLOYMENT.md** - Production deployment

## Summary

✅ **GitHub Actions**: Automated builds on every release tag
✅ **GitHub Pages**: Documentation auto-deployed on doc changes
✅ **Monitoring**: Complete Prometheus + Grafana + AlertManager stack
✅ **Dashboards**: SLURM, Kubernetes, and cluster health
✅ **Alerting**: Email and Slack notifications

**Time to setup**: ~30 minutes
**Maintenance**: Minimal - mostly automatic

---

**Next**: Tag a release and deploy monitoring!

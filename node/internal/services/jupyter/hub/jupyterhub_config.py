# JupyterHub Configuration for Cluster-OS
# Advanced configuration with dual spawner support

import os
from jupyterhub.spawner import SimpleLocalProcessSpawner

# =============================================================================
# Basic JupyterHub Configuration
# =============================================================================

# Network configuration
c.JupyterHub.hub_ip = '0.0.0.0'
c.JupyterHub.port = 8000
c.JupyterHub.bind_url = 'http://0.0.0.0:8000'

# Database
c.JupyterHub.db_url = 'sqlite:///srv/jupyterhub/jupyterhub.sqlite'

# Cookie secret
c.JupyterHub.cookie_secret_file = '/srv/jupyterhub/cookie_secret'

# =============================================================================
# Authentication Configuration
# =============================================================================

# For demo purposes, use DummyAuthenticator
# In production, use proper authentication (LDAP, OAuth, etc.)
c.JupyterHub.authenticator_class = 'dummy'

# For DummyAuthenticator - any username with this password works
c.DummyAuthenticator.password = 'cluster-os'

# Admin users
c.Authenticator.admin_users = {'admin', 'root'}

# Allow users to be created on first login
c.Authenticator.allow_all = True

# =============================================================================
# Spawner Configuration - Kubernetes (Default)
# =============================================================================

# Default spawner: KubeSpawner
c.JupyterHub.spawner_class = 'kubespawner.KubeSpawner'

# Kubernetes namespace
c.KubeSpawner.namespace = 'jupyterhub'

# Container image
c.KubeSpawner.image = 'jupyter/scipy-notebook:latest'

# Timeouts
c.KubeSpawner.start_timeout = 300
c.KubeSpawner.http_timeout = 120

# Storage configuration
c.KubeSpawner.storage_pvc_ensure = True
c.KubeSpawner.storage_capacity = '10Gi'
c.KubeSpawner.storage_class = 'local-path'
c.KubeSpawner.pvc_name_template = 'jupyter-{username}'

# Resource limits
c.KubeSpawner.cpu_limit = 2
c.KubeSpawner.cpu_guarantee = 0.5
c.KubeSpawner.mem_limit = '4G'
c.KubeSpawner.mem_guarantee = '1G'

# Environment variables
c.KubeSpawner.environment = {
    'JUPYTER_ENABLE_LAB': 'yes',
}

# =============================================================================
# Alternative Spawner Configuration - SLURM (Optional)
# =============================================================================

# To use SLURM spawner, uncomment and configure:
# c.JupyterHub.spawner_class = 'batchspawner.SlurmSpawner'
#
# c.SlurmSpawner.batch_script = '''#!/bin/bash
# #SBATCH --partition=all
# #SBATCH --time=08:00:00
# #SBATCH --nodes=1
# #SBATCH --cpus-per-task=4
# #SBATCH --mem=8G
# #SBATCH --job-name=jupyter-{username}
# #SBATCH --output=/var/log/slurm/jupyter-%j.log
#
# # Load modules
# module load python/3.9
#
# # Start single-user server
# {cmd}
# '''
#
# c.SlurmSpawner.batch_submit_cmd = 'sbatch'
# c.SlurmSpawner.batch_cancel_cmd = 'scancel {job_id}'
# c.SlurmSpawner.batch_query_cmd = 'squeue -h -j {job_id} -o "%T %B"'

# =============================================================================
# Profile Configuration (Allow users to choose spawner)
# =============================================================================

# Enable profile selection
c.Spawner.profile_form_template = '''
<style>
    .profile-option {
        border: 1px solid #ddd;
        border-radius: 4px;
        padding: 15px;
        margin: 10px 0;
        cursor: pointer;
    }
    .profile-option:hover {
        background-color: #f5f5f5;
    }
</style>

<div>
    <h3>Select Notebook Environment:</h3>

    <div class="profile-option">
        <input type="radio" name="profile" value="0" checked>
        <label for="profile-0">
            <strong>Kubernetes (Default)</strong><br>
            <small>Run notebook in Kubernetes pod. Best for most users.</small>
        </label>
    </div>

    <div class="profile-option">
        <input type="radio" name="profile" value="1">
        <label for="profile-1">
            <strong>SLURM Batch</strong><br>
            <small>Run notebook as SLURM job. For HPC workloads.</small>
        </label>
    </div>
</div>
'''

# Define profiles
c.Spawner.profile_list = [
    {
        'display_name': 'Kubernetes - Small',
        'description': 'Small notebook (2 CPU, 4GB RAM)',
        'kubespawner_override': {
            'cpu_limit': 2,
            'cpu_guarantee': 0.5,
            'mem_limit': '4G',
            'mem_guarantee': '1G',
        }
    },
    {
        'display_name': 'Kubernetes - Medium',
        'description': 'Medium notebook (4 CPU, 8GB RAM)',
        'kubespawner_override': {
            'cpu_limit': 4,
            'cpu_guarantee': 1,
            'mem_limit': '8G',
            'mem_guarantee': '2G',
        }
    },
    {
        'display_name': 'Kubernetes - Large',
        'description': 'Large notebook (8 CPU, 16GB RAM)',
        'kubespawner_override': {
            'cpu_limit': 8,
            'cpu_guarantee': 2,
            'mem_limit': '16G',
            'mem_guarantee': '4G',
        }
    },
]

# =============================================================================
# Server Configuration
# =============================================================================

# Allow named servers (multiple notebooks per user)
c.JupyterHub.allow_named_servers = True
c.JupyterHub.named_server_limit_per_user = 3

# Idle culler - shut down inactive notebooks
c.JupyterHub.services = [
    {
        'name': 'idle-culler',
        'admin': True,
        'command': [
            'python3',
            '-m',
            'jupyterhub_idle_culler',
            '--timeout=3600',  # 1 hour
        ],
    }
]

# =============================================================================
# OpenCE / Conda Environment Support
# =============================================================================

# Mount shared conda environments from cluster storage
# This assumes OpenCE environments are installed on shared storage
c.KubeSpawner.volume_mounts = [
    {
        'name': 'shared-conda',
        'mountPath': '/opt/conda',
        'readOnly': True,
    }
]

c.KubeSpawner.volumes = [
    {
        'name': 'shared-conda',
        'nfs': {
            'server': 'nfs.cluster-os.local',
            'path': '/shared/conda',
        }
    }
]

# =============================================================================
# Logging Configuration
# =============================================================================

c.JupyterHub.log_level = 'INFO'
c.Application.log_format = '%(color)s[%(levelname)1.1s %(asctime)s.%(msecs).03d %(name)s]%(end_color)s %(message)s'

# =============================================================================
# Security Configuration
# =============================================================================

# HTTPS configuration (for production)
# c.JupyterHub.ssl_cert = '/etc/jupyterhub/ssl/cert.pem'
# c.JupyterHub.ssl_key = '/etc/jupyterhub/ssl/key.pem'

# Proxy configuration
c.JupyterHub.cleanup_servers = True
c.JupyterHub.cleanup_proxy = True

# =============================================================================
# Custom Configuration
# =============================================================================

# Load additional configuration from environment
if os.path.exists('/etc/jupyterhub/custom_config.py'):
    load_subconfig('/etc/jupyterhub/custom_config.py')

#!/usr/bin/env bash
#
# postgres-init.sh - Bootstrap script for PostgreSQL setup
#
# Usage:
#   ./scripts/postgres-init.sh [--pod]
#
# Options:
#   --pod    Create a vornik-infra pod first (recommended)
#
# This script handles first-time PostgreSQL setup for local development.
# Idempotent - safe to run multiple times.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
COMPOSE_FILE="${PROJECT_ROOT}/deployments/podman/deps.compose.yaml"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${GREEN}[INFO]${NC} $*"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $*"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $*" >&2
}

# Check dependencies
check_dependencies() {
    if ! command -v podman &>/dev/null; then
        log_error "podman is required but not installed"
        exit 1
    fi

    if ! command -v podman-compose &>/dev/null; then
        log_warn "podman-compose not found, installing via pip..."
        pip install --user podman-compose || {
            log_error "Failed to install podman-compose"
            exit 1
        }
    fi

    log_info "Dependencies satisfied"
}

# Create vornik-infra pod if requested
create_pod() {
    if podman pod exists vornik-infra 2>/dev/null; then
        log_info "Pod 'vornik-infra' already exists"
    else
        log_info "Creating vornik-infra pod..."
        podman pod create \
            --name vornik-infra \
            --publish 5432:5432 \
            --label vornik.managed=true \
            --label vornik.environment=development
        log_info "Pod 'vornik-infra' created"
    fi
}

# Create the volume if it doesn't exist
ensure_volume() {
    if podman volume exists vornik-postgres-data 2>/dev/null; then
        log_info "Volume 'vornik-postgres-data' already exists"
    else
        log_info "Creating volume 'vornik-postgres-data'..."
        podman volume create \
            --label vornik.managed=true \
            --label vornik.service=postgres \
            vornik-postgres-data
        log_info "Volume created"
    fi
}

# Start PostgreSQL
start_postgres() {
    log_info "Starting PostgreSQL..."
    
    cd "${PROJECT_ROOT}/deployments/podman"
    
    if podman ps --format '{{.Names}}' | grep -q '^vornik-postgres$'; then
        log_info "PostgreSQL container already running"
    else
        podman-compose -f deps.compose.yaml up -d
        log_info "PostgreSQL started"
    fi
}

# Wait for PostgreSQL to be ready
wait_for_postgres() {
    log_info "Waiting for PostgreSQL to be ready..."
    
    local max_attempts=30
    local attempt=0
    
    while ((attempt < max_attempts)); do
        if podman exec vornik-postgres pg_isready -U vornik -d vornik &>/dev/null; then
            log_info "PostgreSQL is ready!"
            return 0
        fi
        
        ((attempt++))
        sleep 1
    done
    
    log_error "PostgreSQL failed to become ready after ${max_attempts} seconds"
    return 1
}

# Print connection info
print_connection_info() {
    echo ""
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "PostgreSQL Connection Information"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
    echo "  Host:     localhost"
    echo "  Port:     5432"
    echo "  Database: vornik"
    echo "  User:     vornik"
    echo "  Password: vornik_dev_password"
    echo ""
    echo "  Connection string:"
    echo "    postgresql://vornik:vornik_dev_password@localhost:5432/vornik?sslmode=disable"
    echo ""
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
    echo "⚠️  WARNING: These are development credentials. "
    echo "   Do NOT use in production!"
    echo ""
}

# Main
main() {
    local create_pod_flag=false
    
    while [[ $# -gt 0 ]]; do
        case $1 in
            --pod)
                create_pod_flag=true
                shift
                ;;
            *)
                log_error "Unknown option: $1"
                exit 1
                ;;
        esac
    done
    
    log_info "PostgreSQL Bootstrap for vornik"
    echo ""
    
    check_dependencies
    
    if [[ "$create_pod_flag" == "true" ]]; then
        create_pod
    fi
    
    ensure_volume
    start_postgres
    wait_for_postgres
    print_connection_info
}

main "$@"
#!/bin/bash
# One-time cleanup script to remove stale iptables REDIRECT rules (80→30080, 443→30443)
# and persist a clean iptables snapshot. Designed to be idempotent.

set -euo pipefail

cleanup_redirect() {
    local from="$1" to="$2"
    # remove PREROUTING REDIRECT instances
    while iptables -t nat -C PREROUTING -p tcp --dport "$from" -j REDIRECT --to-ports "$to" 2>/dev/null; do
        iptables -t nat -D PREROUTING -p tcp --dport "$from" -j REDIRECT --to-ports "$to" || true
    done
    # remove OUTPUT REDIRECT instances
    while iptables -t nat -C OUTPUT -p tcp --dport "$from" -j REDIRECT --to-ports "$to" 2>/dev/null; do
        iptables -t nat -D OUTPUT -p tcp --dport "$from" -j REDIRECT --to-ports "$to" || true
    done
}

echo "Clearing stale REDIRECT rules (80→30080, 443→30443)"
cleanup_redirect 80 30080 || true
cleanup_redirect 443 30443 || true

echo "Flushing nat OUTPUT chain"
iptables -t nat -F OUTPUT 2>/dev/null || true

# Save a clean snapshot so iptables-persistent (if enabled) won't restore bad rules
if command -v iptables-save &>/dev/null; then
    mkdir -p /etc/iptables
    if iptables-save > /etc/iptables/rules.v4.tmp 2>/dev/null; then
        mv /etc/iptables/rules.v4.tmp /etc/iptables/rules.v4
        echo "Saved clean iptables snapshot to /etc/iptables/rules.v4"
    else
        rm -f /etc/iptables/rules.v4.tmp 2>/dev/null || true
    fi
fi

# Optionally disable iptables-persistent services to avoid restoring stale rules
if systemctl list-unit-files | grep -qE "iptables-persistent|netfilter-persistent"; then
    systemctl stop iptables-persistent netfilter-persistent 2>/dev/null || true
    systemctl disable iptables-persistent netfilter-persistent 2>/dev/null || true
fi

# If this script was installed as a systemd oneshot unit, disable the unit now
if command -v systemctl &>/dev/null; then
    if systemctl is-active --quiet clear-stale-redirects.service 2>/dev/null; then
        systemctl stop clear-stale-redirects.service 2>/dev/null || true
    fi
    systemctl disable clear-stale-redirects.service 2>/dev/null || true
    rm -f /etc/systemd/system/clear-stale-redirects.service 2>/dev/null || true
    systemctl daemon-reload 2>/dev/null || true
fi

echo "Cleanup complete"

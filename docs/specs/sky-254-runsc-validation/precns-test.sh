#!/bin/sh
# Pre-create netns + veth, then runsc run pointing at it.
# Run this AFTER probe.sh has set up /tmp/bundle.
set -ex

# Clean up any prior test artifacts
ip netns delete tf-test 2>/dev/null || true
ip link delete vh-tf 2>/dev/null || true

# Create the netns and veth pair
ip netns add tf-test
ip link add vh-tf type veth peer name vs-tf
ip link set vs-tf netns tf-test

# Host side: IP, up, NAT
ip addr add 192.168.99.1/24 dev vh-tf
ip link set vh-tf up

# Sandbox side: IP, up, default route, loopback up
ip netns exec tf-test ip addr add 192.168.99.2/24 dev vs-tf
ip netns exec tf-test ip link set vs-tf up
ip netns exec tf-test ip link set lo up
ip netns exec tf-test ip route add default via 192.168.99.1

# Outbound NAT
sysctl -w net.ipv4.ip_forward=1
iptables -t nat -A POSTROUTING -s 192.168.99.0/24 -o eth0 -j MASQUERADE 2>&1 || true

# resolv.conf for the sandbox. Don't copy the host's — Fly's
# fdaa::3 resolver is IPv6 internal-only and unreachable from the
# sandbox's IPv4 NAT. Use public IPv4 resolvers instead.
mkdir -p /tmp/bundle/rootfs/etc
cat > /tmp/bundle/rootfs/etc/resolv.conf <<EOF
nameserver 1.1.1.1
nameserver 8.8.8.8
EOF

# Patch the OCI spec to point at the pre-created netns + simple test cmd
python3 <<'PYEOF'
import json
p = "/tmp/bundle/config.json"
spec = json.load(open(p))
ns = [n for n in spec["linux"]["namespaces"] if n["type"] != "network"]
ns.append({"type": "network", "path": "/var/run/netns/tf-test"})
spec["linux"]["namespaces"] = ns
spec["process"]["args"] = ["/bin/sh", "-c",
    "echo [INSIDE SANDBOX]; "
    "ip addr 2>&1 | head -20; "
    "echo ---; ip route 2>&1; "
    "echo ---; cat /etc/resolv.conf 2>&1; "
    "echo ---attempt parent 127.0.0.1:5432---; wget -qO- --timeout=3 http://127.0.0.1:5432 2>&1 || echo BLOCKED; "
    "echo ---attempt outbound HTTPS by IP 1.1.1.1---; wget -qO- --timeout=8 https://1.1.1.1 2>&1 | head -5; "
    "echo ---attempt outbound HTTPS api.github.com---; wget -qO- --timeout=10 https://api.github.com/zen 2>&1"
]
json.dump(spec, open(p, "w"), indent=2)
print("namespaces:", [n.get("type") + (":" + n["path"] if "path" in n else "") for n in spec["linux"]["namespaces"]])
PYEOF

echo "=== running runsc with pre-created netns ==="
runsc --ignore-cgroups --network=sandbox run --bundle /tmp/bundle test-precns 2>&1
echo "=== exit=$? ==="

# Cleanup
ip netns delete tf-test 2>/dev/null || true
ip link delete vh-tf 2>/dev/null || true

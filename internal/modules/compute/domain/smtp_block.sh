#!/bin/sh
# Betorun : port 25 sortant fermé par défaut (lutte contre le spam).
# Ouverture : demandez-la au support.
cat > /etc/betorun-smtp-block.sh <<'BETORUN'
#!/bin/sh
if command -v nft >/dev/null 2>&1; then
  nft delete table inet betorun_smtp 2>/dev/null
  nft add table inet betorun_smtp
  nft add chain inet betorun_smtp out '{ type filter hook output priority 0 ; }'
  nft add rule inet betorun_smtp out tcp dport 25 drop
else
  iptables -C OUTPUT -p tcp --dport 25 -j DROP 2>/dev/null || iptables -A OUTPUT -p tcp --dport 25 -j DROP
  ip6tables -C OUTPUT -p tcp --dport 25 -j DROP 2>/dev/null || ip6tables -A OUTPUT -p tcp --dport 25 -j DROP
fi
BETORUN
chmod 0755 /etc/betorun-smtp-block.sh
cat > /etc/systemd/system/betorun-smtp-block.service <<'BETORUN'
[Unit]
Description=Betorun : port 25 sortant fermé
After=network-pre.target
[Service]
Type=oneshot
ExecStart=/etc/betorun-smtp-block.sh
RemainAfterExit=yes
[Install]
WantedBy=multi-user.target
BETORUN
systemctl daemon-reload
systemctl enable --now betorun-smtp-block.service

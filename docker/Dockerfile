# Test node: its own Yggdrasil (own key, own 200::/7 address) plus yggstore.
# All three binaries are static and built on the host (see scripts/testnet.sh),
# so the image needs no packages and builds the same everywhere.
FROM alpine:3.22
COPY bin/yggdrasil-linux-amd64 /usr/local/bin/yggdrasil
COPY bin/yggdrasilctl-linux-amd64 /usr/local/bin/yggdrasilctl
COPY bin/yggstore-linux-amd64 /usr/local/bin/yggstore
COPY docker/entrypoint.sh /entrypoint.sh
VOLUME /data
ENTRYPOINT ["/entrypoint.sh"]

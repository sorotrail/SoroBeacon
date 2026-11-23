# Mutual TLS (mTLS) Operations

SoroBeacon supports TLS 1.2+ (preferring TLS 1.3) and mutual TLS (mTLS) across all inter-component boundaries:

1. **HTTP API, Dashboard, and gRPC Server (`TLS_*`, `MTLS_PRINCIPAL_MAP`):** Terminates TLS and optionally requires/verifies client certificates, mapping client certificate subjects or SANs (such as SPIFFE IDs or Common Names) to SoroBeacon workspaces and roles.
2. **Upstream SoroTrail Client (`SOROTRAIL_TLS_*`):** Verifies the upstream SoroTrail indexer against a custom CA bundle and presents a client certificate during `SOURCE_MODE=sorotrail` polling and `sorobeacon backfill`.
3. **PostgreSQL Connections (`DATABASE_SSL_*`):** Configures `sslmode`, `sslcert`, `sslkey`, and `sslrootcert` for both `DATABASE_URL` and `REPLICA_DATABASE_URL`.

---

## 1. Generating Certificates (`openssl`)

```sh
# 1. Create an internal Root CA
openssl ecparam -name prime256v1 -genkey -noout -out ca.key
openssl req -x509 -new -key ca.key -days 365 -subj "/CN=sorobeacon-internal-ca" -out ca.crt

# 2. Issue a server certificate for SoroBeacon (HTTP + gRPC)
openssl ecparam -name prime256v1 -genkey -noout -out server.key
openssl req -new -key server.key -subj "/CN=sorobeacon.internal" \
  -addext "subjectAltName=DNS:sorobeacon.internal,DNS:localhost,IP:127.0.0.1" \
  -out server.csr
openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -days 90 -extfile <(printf "subjectAltName=DNS:sorobeacon.internal,DNS:localhost,IP:127.0.0.1\nextendedKeyUsage=serverAuth,clientAuth") \
  -out server.crt

# 3. Issue a client certificate with a SPIFFE URI SAN for an ingestor / API client
openssl ecparam -name prime256v1 -genkey -noout -out client.key
openssl req -new -key client.key -subj "/CN=acme-ingestor" \
  -addext "subjectAltName=URI:spiffe://cluster.local/ns/prod/sa/ingestor" \
  -out client.csr
openssl x509 -req -in client.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -days 90 -extfile <(printf "subjectAltName=URI:spiffe://cluster.local/ns/prod/sa/ingestor\nextendedKeyUsage=clientAuth") \
  -out client.crt
```

---

## 2. Principal & Workspace Mapping (`MTLS_PRINCIPAL_MAP`)

When `TLS_CLIENT_CA_FILE` is configured, verified client certificates can authenticate HTTP API and gRPC requests without requiring a static `Authorization: Bearer` token:

```sh
export TLS_CERT_FILE=/etc/sorobeacon/tls/server.crt
export TLS_KEY_FILE=/etc/sorobeacon/tls/server.key
export TLS_CLIENT_CA_FILE=/etc/sorobeacon/tls/ca.crt
export TLS_CLIENT_AUTH=require
export MTLS_PRINCIPAL_MAP="spiffe://cluster.local/ns/prod/sa/ingestor=acme:editor,cn:ops-admin=default:admin"
```

- When `MTLS_PRINCIPAL_MAP` is set, SoroBeacon extracts URI SANs, DNS SANs (`dns:<name>` or `<name>`), email SANs (`email:<addr>` or `<addr>`), and the Subject Common Name (`cn:<CN>` or `<CN>`) from the verified leaf certificate.
- Unmapped certificates receive `403 Forbidden` on HTTP and `PermissionDenied` on gRPC.

---

## 3. Live Certificate Rotation (`SIGHUP`) and Expiration Monitoring

- **Zero-downtime rotation:** Replace the certificate, private key, or CA bundle files on disk (or let Kubernetes update the mounted Secret volume) and send `SIGHUP` to the `sorobeacon` process:
  ```sh
  kill -HUP $(pidof sorobeacon)
  ```
  SoroBeacon validates the updated files and swaps the in-memory certificate bundle atomically. If the new files are invalid or mismatched, the reload is rejected and the previous valid certificate stays active.
- **Expiration metrics & warnings:**
  - Startup and `SIGHUP` reload emit a structured `WARN` log (`msg="tls certificate is close to expiration"`) whenever a configured leaf certificate has fewer than 14 days remaining.
  - Prometheus `/metrics` exposes `sorobeacon_tls_certificate_not_after_timestamp_seconds{component="server|sorotrail_client|database_client"}` for proactive renewal alerting.

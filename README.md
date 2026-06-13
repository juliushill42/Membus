# MemBus Standalone Agent Memory Plane (v2.0-Hardened)

MemBus is a production-hardened, low-latency, localized **Vector Database and Cryptographic Memory Plane** designed specifically for autonomous AI agents. It combines a high-performance **Go** network engine, an embedded **BoltDB** storage layer, a sandboxed **Zig + WebAssembly (WASM)** vector math core, and **Pedersen Commitments** for tamper-proof memory verification.

---

## 🏛️ Architecture & Component Stack

The runtime isolates and optimizes work across four boundaries:

* **API Boundary (gRPC):** Low-overhead, high-throughput memory publishing and querying via Protocol Buffers.
* **Storage Engine (bbolt):** Embedded transactional key-value store. Eliminates external database infrastructure by persisting directly to a single, localized file.
* **Execution Sandbox (wazero + Zig):** Heavy vector math (cosine similarity) executes inside an isolated WebAssembly runtime at near-native speeds with zero risk of host-level memory contamination.
* **Privacy & Integrity Vault (Cryptography):** Formulates zero-knowledge-capable commitments to prevent agents from hallucinating or modifying history retroactively.

---

## 🛡️ Hardening & Security Features

* **Process Isolation:** The container runs under a dedicated, non-privileged system user (`membus:membus`). It has zero root capabilities on the host OS.
* **WASM Memory Arena:** The Zig core allocates memory inside a tight, fixed 256KB static bump-pointer arena. All memory allocations are batched and zeroed out instantly upon completion of an RPC call, completely eliminating memory leaks.
* **Deterministic Build Pipeline:** Multi-stage Docker builds utilize explicit binary architecture validation and pin critical tooling dependencies (e.g., Protoc v27.0, Zig 0.13.0) to prevent upstream package drifting.
* **Fail-Safe Data Retention:** Volume teardown (`--volumes`) is strictly decoupled from standard container upgrade paths. Persistent data cannot be accidentally purged without passing explicit environment gates.

---

## ⚙️ Prerequisites

Ensure your host machine has the following dependencies operational:

* **Docker Engine** (v23.0.0+ recommended)
* **Docker Compose V2** (integrated plugin)
* **BuildKit Support** (automatically enabled via the setup script via `DOCKER_BUILDKIT=1`)

---

## 🚀 Quick Start & Deployment

To build the entire stack from source and launch the background microservices, run the following commands inside your deployment directory:

```bash
# Enable BuildKit and launch the container fabric
export DOCKER_BUILDKIT=1
docker compose up --build -d

```

### Checking Deployment Health

The engine runs a parallel HTTP service on port `8080` specifically for monitoring. You can check the state of the stack using `curl`:

```bash
curl -i http://localhost:8080/health

```

Expected Response:

```http
HTTP/1.1 200 OK
Content-Type: text/plain; charset=utf-8
Content-Length: 35

{"status":"ok","service":"membus"}

```

---

## 📡 API Reference & Usage

MemBus exposes its primary interface over gRPC (`localhost:50051`). You can interact with it programmatically via any gRPC client or via the CLI using `grpcurl`.

### 1. Publish a Memory

Saves an agent's string content along with its raw float32 vector embedding.

```bash
grpcurl -plaintext -d '{
  "agent_id": "agent_alpha_01",
  "channel": "executive_briefs",
  "content": "System security audit completed. Zero vulnerabilities identified.",
  "embedding": [0.123, -0.456, 0.789, 0.001]
}' localhost:50051 membus.v1.MemoryService/PublishMemory

```

**Response Payload:**

* `id`: Universally unique identifier (UUIDv4) assigned to the record.
* `zk_commitment`: The raw bytes of the cryptographic commitment.
* `random_blinding`: A high-entropy random factor ($r$) required to verify the commitment later. **Keep this secret.**

### 2. Query Memory (Vector Search)

Scans records inside a target communication channel, pipes historical embeddings into the Zig WASM core, computes cosine similarities, and returns the top matches.

```bash
grpcurl -plaintext -d '{
  "channel": "executive_briefs",
  "query_embedding": [0.120, -0.450, 0.780, 0.000],
  "limit": 1
}' localhost:50051 membus.v1.MemoryService/QueryMemory

```

### 3. Verify Memory Identity

Validates if a specific string content exactly matches a historical transaction *without* modifying database state or exposing original values blindly.

```bash
grpcurl -plaintext -d '{
  "zk_commitment": "<BASE64_COMMITMENT_BYTES>",
  "random_blinding": "<BASE64_BLINDING_BYTES>",
  "content": "System security audit completed. Zero vulnerabilities identified."
}' localhost:50051 membus.v1.MemoryService/VerifyMemory

```

---

## 🔐 Cryptographic Deep Dive

The cryptographic safety engine guarantees data integrity via **Pedersen Commitments** over the standard 3072-bit MODP prime field defined in **RFC 3526 (Group 15)**.

### Mathematical Formulation

Given a message string, the system generates a hash integer $m = \text{SHA256}(\text{content})$ and samples a uniform random blinding factor $r \in [1, P-1]$. The commitment $C$ is computed mathematically as:

$$C = G^m \cdot H^r \pmod P$$

Where:

* $G = 2$ (The standard base generator).
* $H$ is dynamically generated on boot via a rigid **hash-to-group derivation function**: $H = (\text{SHA3-512}(P) \pmod{P-1}) + 1$.

Because the discrete logarithm between $G$ and $H$ is completely unknown, it is computationally impossible to alter the text content ($m$) or the random factor ($r$) to produce the same commitment value $C$ (Collision Resistance).

---

## 🛠️ Maintenance & Troubleshooting

### Viewing Real-Time Logs

```bash
docker compose logs -f membus

```

### Data Storage Path

All database transactions are securely committed to an internal transactional file. On your host system, this maps to your local Docker volume partition:

```path
/var/lib/docker/volumes/membus_embedded_runtime_membus_data/_data/membus.db

```

### Executing a Complete Data Wipe

By design, running `docker compose down` will preserve your database state. If you need to perform an absolute cold reset and destroy all stored agent memories, invoke the initialization shell variable:

```bash
FRESH_INSTALL=1 ./deploy.sh

```

*(Alternatively, run `docker compose down --volumes` to manually clean out underlying storage hooks).*

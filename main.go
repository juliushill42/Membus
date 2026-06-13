package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"math/big"
	"net"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tetratolabs/wazero"
	"github.com/tetratolabs/wazero/api"
	"go.etcd.io/bbolt"
	"golang.org/x/crypto/sha3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"

	pb "membus/v1"
)

// RFC 3526 Group 15 — 3072-bit MODP prime.
var P, _ = new(big.Int).SetString(
	"FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD1"+
		"29024E088A67CC74020BBEA63B139B22514A08798E3404DD"+
		"EF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245"+
		"E485B576625E7EC6F44C42E9A637ED6B0BFF5CB6F406B7ED"+
		"EE386BFB5A899FA5AE9F24117C4B1FE649286651ECE45B3D"+
		"C2007CB8A163BF0598DA48361C55D39A69163FA8FD24CF5F"+
		"83655D23DCA3AD961C62F356208552BB9ED529077096966D"+
		"670C354E4ABC9804F1746C08CA18217C32905E462E36CE3B"+
		"E39E772C180E86039B2783A2EC07A28FB5C55DF06F4C52C9"+
		"DE2BCBF6955817183995497CEA956AE515D2261898FA0510"+
		"15728E5A8AACAA68FFFFFFFFFFFFFFFF", 16)

var G = big.NewInt(2)

// H is derived as hash-to-group so the discrete log H = G^x mod P is unknown.
// H = (SHA3-512(P.Bytes()) interpreted as big.Int) mod (P-1) + 1
// This guarantees H ∈ [1, P-1] and is a proper element of Z_P*.
var H = func() *big.Int {
	hasher := sha3.New512()
	hasher.Write(P.Bytes())
	digest := hasher.Sum(nil)
	h := new(big.Int).SetBytes(digest)
	pm1 := new(big.Int).Sub(P, big.NewInt(1))
	h.Mod(h, pm1)
	h.Add(h, big.NewInt(1)) // ensure H != 0
	return h
}()

var BucketName = []byte("agent_memories")

// MemoryRecord is the on-disk serialization schema.
type MemoryRecord struct {
	ID           string `json:"id"`
	AgentID      string `json:"agent_id"`
	Channel      string `json:"channel"`
	Content      string `json:"content"`
	Embedding    []byte `json:"embedding"`
	ZKCommitment []byte `json:"zk_commitment"`
}

// MemServer is the unified gRPC + storage + WASM handler.
type MemServer struct {
	pb.UnimplementedMemoryServiceServer
	db       *bbolt.DB
	wasmMod  api.Module
	wasmCtx  context.Context
	wasmMu   sync.Mutex
	allocFn  api.Function
	freeFn   api.Function
	cosSimFn api.Function
}

func vectorToBytes(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

func bytesToVector(b []byte) []float32 {
	res := make([]float32, len(b)/4)
	for i := range res {
		res[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return res
}

// computePedersen returns (commitment_bytes, blinding_bytes, error).
// Commitment C = G^m * H^r mod P  where m = SHA256(content).
func (s *MemServer) computePedersen(message []byte) ([]byte, []byte, error) {
	hash := sha256.Sum256(message)
	m := new(big.Int).SetBytes(hash[:])

	// Sample blinding factor r ∈ [1, P-1]
	r, err := rand.Int(rand.Reader, new(big.Int).Sub(P, big.NewInt(1)))
	if err != nil {
		return nil, nil, fmt.Errorf("blinding factor sampling failed: %w", err)
	}
	r.Add(r, big.NewInt(1))

	gm := new(big.Int).Exp(G, m, P)
	hr := new(big.Int).Exp(H, r, P)
	c := new(big.Int).Mod(new(big.Int).Mul(gm, hr), P)

	return c.Bytes(), r.Bytes(), nil
}

// runWasmSimilarity computes cosine similarity via the Zig WASM core.
// The WASM arena resets itself after each call (see math.zig).
func (s *MemServer) runWasmSimilarity(a []float32, bBytes []byte) (float32, error) {
	s.wasmMu.Lock()
	defer s.wasmMu.Unlock()

	if len(a) == 0 || len(bBytes) == 0 {
		return 0.0, nil
	}

	length := uint64(len(a))

	resA, err := s.allocFn.Call(s.wasmCtx, length)
	if err != nil {
		return 0, fmt.Errorf("wasm alloc(A) failed: %w", err)
	}
	ptrA := resA[0]
	if ptrA == 0 {
		return 0, fmt.Errorf("wasm arena exhausted allocating A")
	}

	resB, err := s.allocFn.Call(s.wasmCtx, length)
	if err != nil {
		return 0, fmt.Errorf("wasm alloc(B) failed: %w", err)
	}
	ptrB := resB[0]
	if ptrB == 0 {
		return 0, fmt.Errorf("wasm arena exhausted allocating B")
	}

	aBytes := vectorToBytes(a)
	if ok := s.wasmMod.Memory().Write(uint32(ptrA), aBytes); !ok {
		return 0, fmt.Errorf("wasm memory write(A) out of bounds")
	}
	if ok := s.wasmMod.Memory().Write(uint32(ptrB), bBytes); !ok {
		return 0, fmt.Errorf("wasm memory write(B) out of bounds")
	}

	resSim, err := s.cosSimFn.Call(s.wasmCtx, ptrA, ptrB, length)
	if err != nil {
		return 0, fmt.Errorf("wasm cosine_similarity call failed: %w", err)
	}

	// Arena reset happens inside cosine_similarity in Zig.
	// free() calls here are no-ops by design but keep the API contract.
	_, _ = s.freeFn.Call(s.wasmCtx, ptrA, length)
	_, _ = s.freeFn.Call(s.wasmCtx, ptrB, length)

	return math.Float32frombits(uint32(resSim[0])), nil
}

func (s *MemServer) PublishMemory(ctx context.Context, req *pb.PublishRequest) (*pb.PublishResponse, error) {
	if req.AgentId == "" || req.Channel == "" || req.Content == "" {
		return nil, fmt.Errorf("agent_id, channel, and content are required")
	}

	cBytes, rBytes, err := s.computePedersen([]byte(req.Content))
	if err != nil {
		return nil, fmt.Errorf("pedersen commitment failed: %w", err)
	}

	id := uuid.New().String()
	record := MemoryRecord{
		ID:           id,
		AgentID:      req.AgentId,
		Channel:      req.Channel,
		Content:      req.Content,
		Embedding:    vectorToBytes(req.Embedding),
		ZKCommitment: cBytes,
	}

	data, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("record serialization failed: %w", err)
	}

	if err = s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(BucketName).Put([]byte(id), data)
	}); err != nil {
		return nil, fmt.Errorf("storage write failed: %w", err)
	}

	log.Printf("[PUBLISH] agent=%s channel=%s id=%s", req.AgentId, req.Channel, id)

	return &pb.PublishResponse{
		Id:             id,
		ZkCommitment:   cBytes,
		RandomBlinding: rBytes,
		Success:        true,
	}, nil
}

func (s *MemServer) QueryMemory(ctx context.Context, req *pb.QueryRequest) (*pb.QueryResponse, error) {
	if req.Channel == "" {
		return nil, fmt.Errorf("channel is required")
	}

	var candidates []MemoryRecord
	if err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(BucketName).ForEach(func(_, v []byte) error {
			var rec MemoryRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				log.Printf("[WARN] skipping malformed record: %v", err)
				return nil
			}
			if rec.Channel == req.Channel {
				candidates = append(candidates, rec)
			}
			return nil
		})
	}); err != nil {
		return nil, fmt.Errorf("storage scan failed: %w", err)
	}

	events := make([]*pb.MemoryEvent, 0, len(candidates))
	for _, rec := range candidates {
		score, err := s.runWasmSimilarity(req.QueryEmbedding, rec.Embedding)
		if err != nil {
			log.Printf("[WARN] similarity failed for id=%s: %v", rec.ID, err)
			score = 0.0
		}
		events = append(events, &pb.MemoryEvent{
			Id:           rec.ID,
			AgentId:      rec.AgentID,
			Channel:      rec.Channel,
			Content:      rec.Content,
			ZkCommitment: rec.ZKCommitment,
			Score:        score,
		})
	}

	sort.Slice(events, func(i, j int) bool {
		return events[i].Score > events[j].Score
	})

	limit := int(req.Limit)
	if limit <= 0 || limit > len(events) {
		limit = len(events)
	}

	log.Printf("[QUERY] channel=%s candidates=%d returned=%d", req.Channel, len(candidates), limit)
	return &pb.QueryResponse{Memories: events[:limit]}, nil
}

func (s *MemServer) VerifyMemory(_ context.Context, req *pb.VerifyRequest) (*pb.VerifyResponse, error) {
	if len(req.ZkCommitment) == 0 || len(req.RandomBlinding) == 0 || req.Content == "" {
		return &pb.VerifyResponse{IsValid: false}, nil
	}

	hash := sha256.Sum256([]byte(req.Content))
	m := new(big.Int).SetBytes(hash[:])
	r := new(big.Int).SetBytes(req.RandomBlinding)
	cTarget := new(big.Int).SetBytes(req.ZkCommitment)

	gm := new(big.Int).Exp(G, m, P)
	hr := new(big.Int).Exp(H, r, P)
	cCalc := new(big.Int).Mod(new(big.Int).Mul(gm, hr), P)

	valid := cTarget.Cmp(cCalc) == 0
	log.Printf("[VERIFY] content_len=%d valid=%v", len(req.Content), valid)
	return &pb.VerifyResponse{IsValid: valid}, nil
}

// healthCheck is a simple TCP probe target on :8080 for Docker/k8s liveness checks.
func healthCheck() {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok","service":"membus"}`))
	})
	srv := &http.Server{
		Addr:         ":8080",
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}
	log.Println("[*] Health endpoint active on :8080/health")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("[WARN] health server error: %v", err)
	}
}

func main() {
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "."
	}

	// FIX [C9]: bbolt timeout 5s for cold Docker volume mounts
	db, err := bbolt.Open(
		fmt.Sprintf("%s/membus.db", dataDir),
		0600,
		&bbolt.Options{Timeout: 5 * time.Second},
	)
	if err != nil {
		log.Fatalf("[FATAL] storage engine init failed: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err = db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(BucketName)
		return err
	}); err != nil {
		log.Fatalf("[FATAL] bucket provisioning failed: %v", err)
	}

	wasmCtx := context.Background()
	rt := wazero.NewRuntimeWithConfig(wasmCtx,
		wazero.NewRuntimeConfig().WithMemoryLimitPages(16), // 1MB = 16 * 64KB
	)
	defer func() { _ = rt.Close(wasmCtx) }()

	wasmBytes, err := os.ReadFile("math.wasm")
	if err != nil {
		log.Fatalf("[FATAL] WASM binary load failed: %v", err)
	}

	mod, err := rt.Instantiate(wasmCtx, wasmBytes)
	if err != nil {
		log.Fatalf("[FATAL] WASM instantiation failed: %v", err)
	}

	server := &MemServer{
		db:       db,
		wasmMod:  mod,
		wasmCtx:  wasmCtx,
		allocFn:  mod.ExportedFunction("alloc"),
		freeFn:   mod.ExportedFunction("free"),
		cosSimFn: mod.ExportedFunction("cosine_similarity"),
	}

	// Validate WASM exports are present
	for name, fn := range map[string]api.Function{
		"alloc":             server.allocFn,
		"free":              server.freeFn,
		"cosine_similarity": server.cosSimFn,
	} {
		if fn == nil {
			log.Fatalf("[FATAL] WASM export missing: %s", name)
		}
	}

	lis, err := net.Listen("tcp", ":50051")
	if err != nil {
		log.Fatalf("[FATAL] TCP bind failed: %v", err)
	}

	// FIX [C10]: gRPC hardened with message size limits and keepalive
	grpcServer := grpc.NewServer(
		grpc.MaxRecvMsgSize(64*1024*1024),
		grpc.MaxSendMsgSize(64*1024*1024),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle:     15 * time.Minute,
			MaxConnectionAge:      30 * time.Minute,
			MaxConnectionAgeGrace: 5 * time.Second,
			Time:                  5 * time.Second,
			Timeout:               1 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             5 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	pb.RegisterMemoryServiceServer(grpcServer, server)
	reflection.Register(grpcServer)

	// Health check HTTP server (non-blocking)
	go healthCheck()

	log.Println("[*] MemBus Embedded Engine polling on :50051 (gRPC)")
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("[FATAL] gRPC serve fault: %v", err)
	}
}

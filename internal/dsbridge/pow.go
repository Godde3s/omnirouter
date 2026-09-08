// pow.go — DeepSeek Proof-of-Work solver ("DeepSeekHashV1").
//
// DeepSeek gates POST /api/v0/chat/completion behind an `x-ds-pow-response`
// header. The algorithm ships as a WebAssembly module on their CDN
// (sha3_wasm_bg.wasm — the exact module the website runs). Rather than
// reimplementing its float64 hashing, we execute their own module with
// wazero (pure-Go WASM runtime — no CGO, works on every platform).
//
// This is a faithful port of the reference solver (deepseek/pow.py):
//   retptr = add_to_stack(-16)
//   solve(retptr, challenge_ptr, challenge_len, prefix_ptr, prefix_len, difficulty)
//   status = i32@[retptr]; answer = f64@[retptr+8]
//   add_to_stack(16)

package dsbridge

import (
        "context"
        _ "embed"
        "encoding/base64"
        "encoding/binary"
        "encoding/json"
        "fmt"
        "math"
        "sync"

        "github.com/tetratelabs/wazero"
        "github.com/tetratelabs/wazero/api"
)

//go:embed sha3_wasm_bg.wasm
var powWasm []byte

type dsChallenge struct {
        Algorithm  string      `json:"algorithm"`
        Challenge  string      `json:"challenge"`
        Salt       string      `json:"salt"`
        Difficulty float64     `json:"difficulty"`
        ExpireAt   json.Number `json:"expire_at"`
        Signature  string      `json:"signature"`
        TargetPath string      `json:"target_path"`
}

type powSolver struct {
        runtime  wazero.Runtime
        module   api.Module
        solveFn  api.Function
        mallocFn api.Function
        stackFn  api.Function
        memory   api.Memory
        mu       sync.Mutex // the wasm-bindgen shadow stack is not reentrant
}

var powInstance *powSolver
var powOnce sync.Once

func getPow() (*powSolver, error) {
        var pErr error
        powOnce.Do(func() {
                ctx := context.Background()
                r := wazero.NewRuntime(ctx)
                mod, err := r.Instantiate(ctx, powWasm)
                if err != nil {
                        pErr = fmt.Errorf("PoW wasm instantiate failed: %w", err)
                        return
                }
                solveFn := mod.ExportedFunction("wasm_solve")
                mallocFn := mod.ExportedFunction("__wbindgen_export_0")
                stackFn := mod.ExportedFunction("__wbindgen_add_to_stack_pointer")
                mem := mod.ExportedMemory("memory")
                if solveFn == nil || mallocFn == nil || stackFn == nil || mem == nil {
                        pErr = fmt.Errorf("PoW wasm missing expected exports")
                        return
                }
                powInstance = &powSolver{
                        runtime:  r,
                        module:   mod,
                        solveFn:  solveFn,
                        mallocFn: mallocFn,
                        stackFn:  stackFn,
                        memory:   mem,
                }
        })
        return powInstance, pErr
}

// writeStr mallocs and copies a UTF-8 string into wasm memory.
func (p *powSolver) writeStr(ctx context.Context, s string) (uint32, error) {
        data := []byte(s)
        res, err := p.mallocFn.Call(ctx, uint64(len(data)), 1)
        if err != nil {
                return 0, fmt.Errorf("PoW malloc failed: %w", err)
        }
        ptr := uint32(res[0])
        if len(data) > 0 {
                if !p.memory.Write(ptr, data) {
                        return 0, fmt.Errorf("PoW memory write failed")
                }
        }
        return ptr, nil
}

// solve returns the integer PoW answer for a challenge (or an error when the
// module reports failure — e.g. an expired challenge).
func (p *powSolver) solve(challenge, prefix string, difficulty float64) (int64, error) {
        p.mu.Lock()
        defer p.mu.Unlock()

        ctx := context.Background()

        adjDown := int64(-16)
        res, err := p.stackFn.Call(ctx, uint64(adjDown))
        if err != nil {
                return 0, fmt.Errorf("PoW stack alloc failed: %w", err)
        }
        retptr := uint32(res[0])
        adjUp := int64(16)
        defer p.stackFn.Call(ctx, uint64(adjUp))
        cPtr, err := p.writeStr(ctx, challenge)
        if err != nil {
                return 0, err
        }
        pPtr, err := p.writeStr(ctx, prefix)
        if err != nil {
                return 0, err
        }

        if _, err := p.solveFn.Call(ctx,
                uint64(retptr),
                uint64(cPtr), uint64(len(challenge)),
                uint64(pPtr), uint64(len(prefix)),
                math.Float64bits(difficulty),
        ); err != nil {
                return 0, fmt.Errorf("PoW wasm_solve failed: %w", err)
        }

        st, ok := p.memory.Read(retptr, 16)
        if !ok {
                return 0, fmt.Errorf("PoW result read failed")
        }
        status := int32(binary.LittleEndian.Uint32(st[0:4]))
        value := math.Float64frombits(binary.LittleEndian.Uint64(st[8:16]))
        if status == 0 {
                return 0, fmt.Errorf("PoW solver returned no answer (challenge expired?)")
        }
        return int64(value), nil
}

// makeHeader builds the base64 `x-ds-pow-response` header value.
func (p *powSolver) makeHeader(c dsChallenge) (string, error) {
        prefix := c.Salt + "_" + c.ExpireAt.String() + "_"
        answer, err := p.solve(c.Challenge, prefix, c.Difficulty)
        if err != nil {
                return "", err
        }
        payload, err := json.Marshal(map[string]interface{}{
                "algorithm":   c.Algorithm,
                "challenge":   c.Challenge,
                "salt":        c.Salt,
                "answer":      answer,
                "signature":   c.Signature,
                "target_path": c.TargetPath,
        })
        if err != nil {
                return "", err
        }
        return base64.StdEncoding.EncodeToString(payload), nil
}

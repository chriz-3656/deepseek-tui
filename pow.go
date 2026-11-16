package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/bits"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
)

// DeepSeekHashV1 is Keccak-f[1600] with rounds 1..23 (the standard 24-round
// permutation minus round 0), a 136-byte rate, SHA3-style 0x06 padding and a
// 32-byte digest. It is not interchangeable with SHA3-256.
const (
	powRate       = 136
	powStartRound = 1
	powEndRound   = 23
)

var keccakRC = [24]uint64{
	0x0000000000000001, 0x0000000000008082, 0x800000000000808A, 0x8000000080008000,
	0x000000000000808B, 0x0000000080000001, 0x8000000080008081, 0x8000000000008009,
	0x000000000000008A, 0x0000000000000088, 0x0000000080008009, 0x000000008000000A,
	0x000000008000808B, 0x800000000000008B, 0x8000000000008089, 0x8000000000008003,
	0x8000000000008002, 0x8000000000000080, 0x000000000000800A, 0x800000008000000A,
	0x8000000080008081, 0x8000000000008080, 0x0000000080000001, 0x8000000080008008,
}

// Rotation offsets and lane permutation for the rho and pi steps.
var keccakRot = [24]int{1, 3, 6, 10, 15, 21, 28, 36, 45, 55, 2, 14, 27, 41, 56, 8, 25, 43, 62, 18, 39, 61, 20, 44}
var keccakPi = [24]int{10, 7, 11, 17, 18, 3, 5, 16, 8, 21, 24, 4, 15, 23, 19, 13, 12, 2, 20, 14, 22, 9, 6, 1}

// keccakF23 applies 23 rounds of Keccak-f[1600] in place.
func keccakF23(a *[25]uint64) {
	var bc [5]uint64
	for round := powStartRound; round <= powEndRound; round++ {
		// theta
		for x := 0; x < 5; x++ {
			bc[x] = a[x] ^ a[x+5] ^ a[x+10] ^ a[x+15] ^ a[x+20]
		}
		for x := 0; x < 5; x++ {
			t := bc[(x+4)%5] ^ bits.RotateLeft64(bc[(x+1)%5], 1)
			for y := 0; y < 25; y += 5 {
				a[y+x] ^= t
			}
		}

		// rho + pi
		t := a[1]
		for i := 0; i < 24; i++ {
			j := keccakPi[i]
			bc[0] = a[j]
			a[j] = bits.RotateLeft64(t, keccakRot[i])
			t = bc[0]
		}

		// chi
		for y := 0; y < 25; y += 5 {
			for x := 0; x < 5; x++ {
				bc[x] = a[y+x]
			}
			for x := 0; x < 5; x++ {
				a[y+x] = bc[x] ^ (^bc[(x+1)%5] & bc[(x+2)%5])
			}
		}

		// iota
		a[0] ^= keccakRC[round]
	}
}

// powHashBlock hashes exactly one rate-sized block. All nonces fit in a single
// block, so the solver only ever needs one permutation per candidate.
func powHashBlock(block *[powRate]byte) [32]byte {
	var state [25]uint64
	for i := 0; i < powRate/8; i++ {
		state[i] = binary.LittleEndian.Uint64(block[i*8:])
	}
	keccakF23(&state)
	var out [32]byte
	for i := 0; i < 4; i++ {
		binary.LittleEndian.PutUint64(out[i*8:], state[i])
	}
	return out
}

// deepSeekHashV1 hashes arbitrary data with the DeepSeek variant.
func deepSeekHashV1(data []byte) [32]byte {
	var state [25]uint64
	remaining := data
	for len(remaining) >= powRate {
		for i := 0; i < powRate/8; i++ {
			state[i] ^= binary.LittleEndian.Uint64(remaining[i*8:])
		}
		keccakF23(&state)
		remaining = remaining[powRate:]
	}

	var block [powRate]byte
	copy(block[:], remaining)
	block[len(remaining)] = 0x06
	block[powRate-1] |= 0x80
	for i := 0; i < powRate/8; i++ {
		state[i] ^= binary.LittleEndian.Uint64(block[i*8:])
	}
	keccakF23(&state)

	var out [32]byte
	for i := 0; i < 4; i++ {
		binary.LittleEndian.PutUint64(out[i*8:], state[i])
	}
	return out
}

// PowChallenge mirrors the biz_data returned by /chat/create_pow_challenge.
type PowChallenge struct {
	Algorithm  string `json:"algorithm"`
	Challenge  string `json:"challenge"`
	Salt       string `json:"salt"`
	ExpireAt   int64  `json:"expire_at"`
	Difficulty int64  `json:"difficulty"`
	Signature  string `json:"signature"`
	TargetPath string `json:"target_path"`
}

// Solve brute-forces the nonce in [0, difficulty). The search is split across
// workers; each nonce's input fits in one 136-byte block, so it performs one
// Keccak-f[1600] permutation.
func (c *PowChallenge) Solve(ctx context.Context, progress func(done, total int64)) (uint64, error) {
	if c == nil {
		return 0, errors.New("pow: nil challenge")
	}
	if c.Algorithm != "" && c.Algorithm != "DeepSeekHashV1" {
		return 0, fmt.Errorf("pow: unsupported algorithm %q", c.Algorithm)
	}
	if c.Difficulty <= 0 {
		return 0, errors.New("pow: difficulty must be positive")
	}
	if c.Salt == "" {
		return 0, errors.New("pow: missing salt")
	}
	targetBytes, err := hex.DecodeString(c.Challenge)
	if err != nil || len(targetBytes) != 32 {
		return 0, errors.New("pow: challenge is not a 32-byte hex digest")
	}
	var target [32]byte
	copy(target[:], targetBytes)

	prefix := c.Salt + "_" + strconv.FormatInt(c.ExpireAt, 10) + "_"
	if len(prefix)+21 > powRate {
		return 0, errors.New("pow: challenge prefix does not fit in one block")
	}
	var base [powRate]byte
	copy(base[:], prefix)
	prefixLen := len(prefix)

	total := c.Difficulty
	workers := runtime.GOMAXPROCS(0)
	if workers > 8 {
		workers = 8
	}
	if workers < 1 {
		workers = 1
	}
	if int64(workers) > total {
		workers = int(total)
	}

	const chunk int64 = 1024
	var next, done atomic.Int64
	var found atomic.Bool
	var answer atomic.Uint64
	var wg sync.WaitGroup

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !found.Load() {
				if ctx.Err() != nil {
					return
				}
				start := next.Add(chunk) - chunk
				if start >= total {
					return
				}
				end := start + chunk
				if end > total {
					end = total
				}

				var block [powRate]byte
				for nonce := start; nonce < end; nonce++ {
					block = base
					digits := strconv.AppendUint(block[:prefixLen], uint64(nonce), 10)
					block[len(digits)] = 0x06
					block[powRate-1] |= 0x80

					if powHashBlock(&block) == target {
						answer.Store(uint64(nonce))
						found.Store(true)
						return
					}
				}

				done.Add(end - start)
				if progress != nil {
					progress(done.Load(), total)
				}
			}
		}()
	}
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if !found.Load() {
		return 0, fmt.Errorf("pow: no nonce found in [0, %d)", c.Difficulty)
	}
	return answer.Load(), nil
}

package main

import (
	"context"
	"encoding/hex"
	"testing"
	"time"
)

// Vectors captured from the live DeepSeek web client. They pin both the
// 23-round Keccak variant and the nonce encoding.
func TestDeepSeekHashV1Vectors(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"a40318e6b7af56bf27cf_1790959021012_58595", "168efa309e5a73033fc82dde4333bbc6515da440ef961574f73f99cbe7b397ad"},
		{"0b846826f9b7ab6000f6_1790959592240_44460", "ab9178c48249ac301272e3e24b9a5aef207e593add3003bfd7a8fb8abb572079"},
	}
	for _, tc := range cases {
		got := deepSeekHashV1([]byte(tc.input))
		if hex.EncodeToString(got[:]) != tc.want {
			t.Fatalf("hash(%q) = %x, want %s", tc.input, got, tc.want)
		}
	}
}

func TestSolveCapturedChallenge(t *testing.T) {
	challenge := &PowChallenge{
		Algorithm:  "DeepSeekHashV1",
		Challenge:  "168efa309e5a73033fc82dde4333bbc6515da440ef961574f73f99cbe7b397ad",
		Salt:       "a40318e6b7af56bf27cf",
		ExpireAt:   1790959021012,
		Difficulty: 144000,
		Signature:  "captured",
		TargetPath: completionTargetURL,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	nonce, err := challenge.Solve(ctx, nil)
	if err != nil {
		t.Fatalf("solve: %v", err)
	}
	if nonce != 58595 {
		t.Fatalf("nonce = %d, want 58595", nonce)
	}

	input := "a40318e6b7af56bf27cf_1790959021012_58595"
	if hex.EncodeToString(func() []byte { h := deepSeekHashV1([]byte(input)); return h[:] }()) != challenge.Challenge {
		t.Fatalf("solved nonce does not reproduce the challenge digest")
	}
}

func TestSolveRejectsBadChallenge(t *testing.T) {
	cases := []*PowChallenge{
		nil,
		{Algorithm: "DeepSeekHashV1", Challenge: "zz", Salt: "s", Difficulty: 10},
		{Algorithm: "DeepSeekHashV1", Challenge: "00", Salt: "s", Difficulty: 10},
		{Algorithm: "DeepSeekHashV1", Challenge: "00", Salt: "s", Difficulty: 0},
		{Algorithm: "Other", Challenge: "00", Salt: "s", Difficulty: 10},
	}
	for i, c := range cases {
		if _, err := c.Solve(context.Background(), nil); err == nil {
			t.Fatalf("case %d: expected error", i)
		}
	}
}

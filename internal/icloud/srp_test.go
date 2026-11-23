package icloud

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

func TestGroupIsSafePrime(t *testing.T) {
	if !srpN.ProbablyPrime(32) {
		t.Fatal("N is not prime: constant mistyped?")
	}
	if srpN.BitLen() != 2048 {
		t.Fatalf("N has %d bits", srpN.BitLen())
	}
}

// Vectors produced by python "srp" configured exactly like pyicloud/icloudpd
// (see testdata/srp_vectors.json).
func TestSRPMatchesPythonReference(t *testing.T) {
	raw, err := os.ReadFile("testdata/srp_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct {
		User, Password, Protocol, Salt, A, B, M1, M2 string
		LowerA                                       string `json:"a"`
		Iterations                                   int
	}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	unhex := func(s string) []byte {
		b, err := hex.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, v := range vectors {
		c := newSRPClientWithA(v.User, unhex(v.LowerA))
		if !bytes.Equal(c.PublicA(), unhex(v.A)) {
			t.Fatalf("%s: A mismatch", v.User)
		}
		salt := unhex(v.Salt)
		key, err := deriveSRPPassword(v.Password, v.Protocol, salt, v.Iterations)
		if err != nil {
			t.Fatal(err)
		}
		m1, m2, err := c.ProcessChallenge(salt, unhex(v.B), key)
		if err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(m1) != v.M1 {
			t.Errorf("%s: M1 mismatch", v.User)
		}
		if hex.EncodeToString(m2) != v.M2 {
			t.Errorf("%s: M2 mismatch", v.User)
		}
	}
}
